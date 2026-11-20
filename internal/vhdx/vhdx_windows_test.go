//go:build windows

package vhdx

import (
	"errors"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procCreateVirtualDisk = virtdisk.NewProc("CreateVirtualDisk")
	procEncryptFileW      = windows.NewLazySystemDLL("advapi32.dll").NewProc("EncryptFileW")
)

// createVirtualDiskParameters is CREATE_VIRTUAL_DISK_PARAMETERS, version 2: the
// shape that pairs with VIRTUAL_DISK_ACCESS_NONE, like the open in this package.
// The union holds a ULONGLONG, so it starts 8-aligned after the version tag.
type createVirtualDiskParameters struct {
	Version                   uint32
	_                         uint32
	UniqueID                  windows.GUID
	MaximumSize               uint64
	BlockSizeInBytes          uint32
	SectorSizeInBytes         uint32
	PhysicalSectorSizeInBytes uint32
	_                         uint32
	ParentPath                *uint16
	SourcePath                *uint16
	OpenFlags                 uint32
	ParentVirtualStorageType  virtualStorageType
	SourceVirtualStorageType  virtualStorageType
	ResiliencyGUID            windows.GUID
}

// newDisk creates an empty dynamic .vhdx, which needs no administrator rights.
func newDisk(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.vhdx")
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	st := virtualStorageType{DeviceID: virtualStorageTypeDeviceVHDX, VendorID: msftVendorGUID}
	params := createVirtualDiskParameters{Version: 2, MaximumSize: 64 << 20}
	var h windows.Handle
	r, _, e := procCreateVirtualDisk.Call(
		uintptr(unsafe.Pointer(&st)),
		uintptr(unsafe.Pointer(p)),
		uintptr(virtualDiskAccessNone),
		0, // default security descriptor
		0, // CREATE_VIRTUAL_DISK_FLAG_NONE: dynamic
		0,
		uintptr(unsafe.Pointer(&params)),
		0,
		uintptr(unsafe.Pointer(&h)),
	)
	if r != 0 {
		t.Fatalf("CreateVirtualDisk: %v", syscallErr(r, e))
	}
	windows.CloseHandle(h)
	return path
}

func TestCompactPlainDisk(t *testing.T) {
	path := newDisk(t)
	if why := Refusal(path); len(why) != 0 {
		t.Fatalf("Refusal = %v on a plain disk", why)
	}
	if _, err := Compact(path); err != nil {
		t.Fatalf("Compact: %v", err)
	}
}

// Windows compacts an EFS-encrypted disk when it is opened with
// OPEN_VIRTUAL_DISK_FLAG_SUPPORT_ENCRYPTED_FILES, which builds before 22631
// reject outright (microsoft/WSL#42008, #42041).
func TestCompactEncryptedDisk(t *testing.T) {
	if !encryptedSupported() {
		t.Skipf("Windows build %d predates encrypted VHD support (%d)",
			windows.RtlGetVersion().BuildNumber, minBuildEncryptedVHD)
	}
	path := newDisk(t)
	p, _ := windows.UTF16PtrFromString(path)
	if r, _, e := procEncryptFileW.Call(uintptr(unsafe.Pointer(p))); r == 0 {
		// Windows Home has no EFS, nor does a volume that is not NTFS.
		t.Skipf("cannot EFS-encrypt %s here: %v", path, e)
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_ENCRYPTED == 0 {
		t.Fatalf("disk is not encrypted after EncryptFileW (attrs %#x, %v)", attrs, err)
	}

	if why := Refusal(path); len(why) != 0 {
		t.Fatalf("Refusal = %v, want none: this build compacts encrypted disks", why)
	}
	if _, err := Compact(path); err != nil {
		var errno windows.Errno
		errors.As(err, &errno)
		t.Fatalf("Compact on an encrypted disk: %v (errno %d)", err, uint32(errno))
	}
}
