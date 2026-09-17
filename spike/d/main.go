//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32                    = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoCreateInstance     = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
	procCoInitializeSecurity = ole32.NewProc("CoInitializeSecurity")
)

// From src/windows/service/inc/wslservice.idl
var (
	clsidLxssUserSession      = windows.GUID{Data1: 0xa9b7a1b9, Data2: 0x0671, Data3: 0x405c, Data4: [8]byte{0x95, 0xf1, 0xe0, 0x61, 0x2c, 0xb4, 0xce, 0x7e}}
	clsidLxssUserSessionInBox = windows.GUID{Data1: 0x4f476546, Data2: 0xb412, Data3: 0x4579, Data4: [8]byte{0xb6, 0x4c, 0x12, 0x3d, 0xf3, 0x31, 0xe3, 0xd6}}
	iidILxssUserSession       = windows.GUID{Data1: 0x38541BDC, Data2: 0xF54F, Data3: 0x4CEB, Data4: [8]byte{0x85, 0xd0, 0x37, 0xf0, 0xf3, 0xd2, 0x61, 0x7e}}
)

const (
	coinitApartmentThreaded = 0x2
	clsctxLocalServer       = 0x4
)

type lxssErrorInfo struct {
	Flags        uint32
	_            uint32
	Context      uint64
	Message      *uint16
	Warnings     *uint16
	WarningsPipe uint32
	_            uint32
}

type lxssEnumerateInfo struct {
	DistroGuid windows.GUID
	State      uint32
	Version    uint32
	Flags      uint32
	DistroName [257]uint16
}

var states = map[uint32]string{
	0: "Invalid", 1: "Installed", 2: "Running", 3: "Installing",
	4: "Uninstalling", 5: "Converting", 6: "Exporting", 7: "Compacting",
}

func main() {
	fmt.Printf("sizeof(LXSS_ENUMERATE_INFO) = %d\n", unsafe.Sizeof(lxssEnumerateInfo{}))
	fmt.Printf("sizeof(LXSS_ERROR_INFO)     = %d\n\n", unsafe.Sizeof(lxssErrorInfo{}))

	// A COM apartment belongs to an OS THREAD, and Go moves goroutines between
	// threads freely. Without this lock, CoCreateInstance below intermittently
	// returns CO_E_NOTINITIALIZED (0x800401F0) because it ran on a thread that
	// never saw CoInitializeEx -- observed in 2 of 4 runs here, right after the
	// process-spawn loop gave the scheduler a reason to migrate.
	//
	// Intermittent, load-dependent, and invisible in a quick test: exactly the
	// shape of bug that ships.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	procCoInitializeEx.Call(0, coinitApartmentThreaded)
	defer procCoUninitialize.Call()

	// RPC_C_AUTHN_LEVEL_DEFAULT=0, RPC_C_IMP_LEVEL_IMPERSONATE=3.
	// Without this the service refuses with ERROR_BAD_IMPERSONATION_LEVEL:
	// it impersonates the caller to act on that user's distros.
	hr, _, _ := procCoInitializeSecurity.Call(0, ^uintptr(0), 0, 0, 0, 3, 0, 0, 0)
	fmt.Printf("CoInitializeSecurity -> 0x%08X\n\n", uint32(hr))

	benchCLI(20)

	for _, c := range []struct {
		name  string
		clsid windows.GUID
	}{{"LxssUserSession (Store)", clsidLxssUserSession}, {"LxssUserSessionInBox", clsidLxssUserSessionInBox}} {
		var sess uintptr
		hr, _, _ := procCoCreateInstance.Call(
			uintptr(unsafe.Pointer(&c.clsid)), 0, clsctxLocalServer,
			uintptr(unsafe.Pointer(&iidILxssUserSession)), uintptr(unsafe.Pointer(&sess)))
		if hr != 0 {
			fmt.Printf("%-26s CoCreateInstance -> 0x%08X\n", c.name, uint32(hr))
			continue
		}
		fmt.Printf("%-26s CoCreateInstance -> OK (%#x)\n", c.name, sess)
		enumerate(sess)
		release(sess)
	}
}

func vtbl(obj uintptr) *[64]uintptr { return *(**[64]uintptr)(unsafe.Pointer(obj)) }
func release(obj uintptr)           { syscallN(vtbl(obj)[2], obj) }

func enumerate(sess uintptr) {
	var count uint32
	var arr *lxssEnumerateInfo
	var errInfo lxssErrorInfo

	start := time.Now()
	hr := syscallN(vtbl(sess)[15], sess,
		uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&arr)),
		uintptr(unsafe.Pointer(&errInfo)))
	elapsed := time.Since(start)

	if hr != 0 {
		fmt.Printf("  EnumerateDistributions -> 0x%08X", uint32(hr))
		if errInfo.Message != nil {
			fmt.Printf("  message=%q", windows.UTF16PtrToString(errInfo.Message))
		}
		fmt.Println()
		return
	}
	fmt.Printf("  EnumerateDistributions -> OK in %v, %d distro(s)\n", elapsed, count)
	slice := unsafe.Slice(arr, count)
	for _, d := range slice {
		fmt.Printf("    %-24s state=%-10s version=%d flags=%#x\n",
			windows.UTF16ToString(d.DistroName[:]), states[d.State], d.Version, d.Flags)
	}
	procCoTaskMemFree.Call(uintptr(unsafe.Pointer(arr)))

	// Timed loop for the benchmark half.
	const n = 20
	t0 := time.Now()
	for i := 0; i < n; i++ {
		var c2 uint32
		var a2 *lxssEnumerateInfo
		var e2 lxssErrorInfo
		if h := syscallN(vtbl(sess)[15], sess,
			uintptr(unsafe.Pointer(&c2)), uintptr(unsafe.Pointer(&a2)), uintptr(unsafe.Pointer(&e2))); h != 0 {
			fmt.Fprintf(os.Stderr, "  iteration %d failed 0x%08X\n", i, uint32(h))
			return
		}
		procCoTaskMemFree.Call(uintptr(unsafe.Pointer(a2)))
	}
	fmt.Printf("  %d calls in %v  (mean %v per call)\n", n, time.Since(t0), time.Since(t0)/n)
}
