//go:build windows

package vhdx

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	virtdisk              = windows.NewLazySystemDLL("virtdisk.dll")
	procOpenVirtualDisk   = virtdisk.NewProc("OpenVirtualDisk")
	procCompactVirtualDsk = virtdisk.NewProc("CompactVirtualDisk")
)

// Win32 constants from virtdisk.h. Only the ones this package needs.
const (
	virtualStorageTypeDeviceVHDX = 3
	openVirtualDiskVersion2      = 2
	// VIRTUAL_DISK_ACCESS_NONE: the only mask the V2 parameters accept, and
	// the reason V2 is used here (see the package comment).
	virtualDiskAccessNone   = 0
	compactVirtualDiskV1    = 1
	openVirtualDiskFlagNone = 0
)

// MSFT vendor GUID, which identifies VHD/VHDX images.
var msftVendorGUID = windows.GUID{
	Data1: 0xEC984AEC,
	Data2: 0xA0F9,
	Data3: 0x47E9,
	Data4: [8]byte{0x90, 0x1F, 0x71, 0x41, 0x5A, 0x66, 0x34, 0x5B},
}

type virtualStorageType struct {
	DeviceID uint32
	VendorID windows.GUID
}

// openVirtualDiskParameters is the V2 shape: a version tag followed by the V2
// union member (GetInfoOnly, ReadOnly, ResiliencyGUID). Laid out explicitly
// rather than with a union type so the size Windows validates is unambiguous.
type openVirtualDiskParameters struct {
	Version        uint32
	_              uint32 // padding to align the union at 8 bytes
	GetInfoOnly    int32
	ReadOnly       int32
	ResiliencyGUID windows.GUID
}

type compactVirtualDiskParameters struct {
	Version  uint32
	Reserved uint32
}

// Compact shrinks the .vhdx at path, returning its size on disk before and
// after. The disk must not be attached: WSL holds every distro's disk open
// while any distro is running, and the caller is expected to have stopped
// things. A disk that is still held fails with ErrInUse and is left unchanged,
// so the caller can retry until WSL lets go of it.
//
// No elevation is required. That is the property the whole feature rests on --
// Optimize-VHD needs the Hyper-V module, which Windows Home does not have.
func Compact(path string) (Result, error) {
	before, err := SizeOnDisk(path)
	if err != nil {
		return Result{}, err
	}

	handle, err := open(path)
	if err != nil {
		return Result{}, err
	}
	defer windows.CloseHandle(handle)

	params := compactVirtualDiskParameters{Version: compactVirtualDiskV1}
	// Synchronous: no OVERLAPPED, so the call returns when compaction is done.
	r, _, e := procCompactVirtualDsk.Call(
		uintptr(handle),
		uintptr(0), // COMPACT_VIRTUAL_DISK_FLAG_NONE
		uintptr(unsafe.Pointer(&params)),
		0, // no OVERLAPPED
	)
	if r != 0 {
		err := syscallErr(r, e)
		// On WSL 3.0 the open succeeds while the utility VM still holds the
		// disk, and the sharing violation arrives here instead (#560).
		if errno, ok := err.(windows.Errno); ok && errno == windows.ERROR_SHARING_VIOLATION {
			return Result{}, fmt.Errorf("%w: %s", ErrInUse, path)
		}
		return Result{}, fmt.Errorf("vhdx: CompactVirtualDisk(%s): %w", path, err)
	}

	after, err := SizeOnDisk(path)
	if err != nil {
		return Result{}, err
	}
	return Result{Before: before, After: after}, nil
}

// open opens the disk with the V2 parameters and VIRTUAL_DISK_ACCESS_NONE.
// Against WSL 2.7 this was the combination that failed at the open rather than
// at the compaction when the disk could not be had; against WSL 3.0.3 the open
// succeeds while the disk is held (#560), so Compact checks both.
func open(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	st := virtualStorageType{DeviceID: virtualStorageTypeDeviceVHDX, VendorID: msftVendorGUID}
	params := openVirtualDiskParameters{Version: openVirtualDiskVersion2}

	var handle windows.Handle
	r, _, e := procOpenVirtualDisk.Call(
		uintptr(unsafe.Pointer(&st)),
		uintptr(unsafe.Pointer(p)),
		uintptr(virtualDiskAccessNone),
		uintptr(openVirtualDiskFlagNone),
		uintptr(unsafe.Pointer(&params)),
		uintptr(unsafe.Pointer(&handle)),
	)
	if r != 0 {
		err := syscallErr(r, e)
		// ERROR_SHARING_VIOLATION is the everyday case: the utility VM still
		// has it. Report it as such so the caller can explain the wait.
		if errno, ok := err.(windows.Errno); ok && errno == windows.ERROR_SHARING_VIOLATION {
			return 0, fmt.Errorf("%w: %s", ErrInUse, path)
		}
		return 0, fmt.Errorf("vhdx: OpenVirtualDisk(%s): %w", path, err)
	}
	return handle, nil
}

// syscallErr turns a Win32 return code into an error. virtdisk functions
// return the error code directly rather than setting last-error, so the return
// value is authoritative and GetLastError is only a fallback.
func syscallErr(ret uintptr, lastErr error) error {
	if ret != 0 {
		return windows.Errno(ret)
	}
	return lastErr
}

// SizeOnDisk returns the number of bytes the file actually occupies on the
// volume, which is what compaction changes. For a sparse or compressed file
// that is smaller than its logical length, and the logical length is exactly
// the number that does not move when a disk is compacted -- so reporting it
// would tell the user nothing happened.
func SizeOnDisk(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var high uint32
	low, err := getCompressedFileSize(p, &high)
	if low == invalidFileSize && err != nil {
		return 0, fmt.Errorf("vhdx: size of %s: %w", path, err)
	}
	return uint64(high)<<32 | uint64(low), nil
}

const invalidFileSize = 0xFFFFFFFF

// Refusal names what about the file makes Windows refuse to compact it, or
// nil. CompactVirtualDisk will not touch a sparse, compressed or encrypted
// file ("Virtual hard disk files must be uncompressed and unencrypted and must
// not be sparse"), and since WSL 3.0.2 `wsl --manage --set-sparse true` makes
// a sparse one without any unsafe flag (#565). Checked up front so the engine
// is not stopped for a compaction that cannot happen. An unreadable attribute
// is not a refusal: the compaction will say what is wrong.
func Refusal(path string) []string {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return nil
	}
	var why []string
	if attrs&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		why = append(why, "sparse")
	}
	if attrs&windows.FILE_ATTRIBUTE_COMPRESSED != 0 {
		why = append(why, "compressed")
	}
	if attrs&windows.FILE_ATTRIBUTE_ENCRYPTED != 0 {
		why = append(why, "encrypted")
	}
	return why
}

var procGetCompressedFileSizeW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

func getCompressedFileSize(name *uint16, high *uint32) (uint32, error) {
	r, _, e := procGetCompressedFileSizeW.Call(
		uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(high)),
	)
	if uint32(r) == invalidFileSize {
		return invalidFileSize, e
	}
	return uint32(r), nil
}
