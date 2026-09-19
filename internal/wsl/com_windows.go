//go:build windows

package wsl

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A COM client for wslservice, used for the one call the supervisor makes on
// every health tick (#356).
//
// `wsl.exe --list --verbose` costs a process spawn, measured at 62-67 ms on the
// spike machine against 0.66-0.81 ms for the equivalent COM call — roughly 85x
// (spike/d). The supervisor pays that every interval for the life of the
// session, and `status` and `doctor` pay it too.
//
// Only EnumerateDistributions is here. CreateLxProcess carries handles, pipes
// and a process lifecycle across the RPC boundary, and spike/d deliberately did
// not touch it; Exec and Start stay on wsl.exe until something establishes it.

// Identifiers from src/windows/service/inc/wslservice.idl in the open-sourced
// WSL. Read, not guessed.
var (
	clsidLxssUserSession = windows.GUID{Data1: 0xa9b7a1b9, Data2: 0x0671, Data3: 0x405c,
		Data4: [8]byte{0x95, 0xf1, 0xe0, 0x61, 0x2c, 0xb4, 0xce, 0x7e}}
	iidILxssUserSession = windows.GUID{Data1: 0x38541BDC, Data2: 0xF54F, Data3: 0x4CEB,
		Data4: [8]byte{0x85, 0xd0, 0x37, 0xf0, 0xf3, 0xd2, 0x61, 0x7e}}
)

// slotEnumerateDistributions is the vtable index of EnumerateDistributions:
// three IUnknown methods, then twelve interface methods before it.
//
// Calling a raw vtable slot is only safe because the IID gates it. COM's
// contract is that an interface's layout is fixed for the lifetime of its IID —
// a changed vtable is a new IID — so QueryInterface succeeding for
// IID_ILxssUserSession is itself the assurance that slot 15 is this method. If
// Microsoft reshapes the interface, CoCreateInstance returns E_NOINTERFACE and
// this whole path is skipped rather than calling something else by mistake.
// That is a stronger guarantee than gating on a WSL version string, which would
// fail closed on every new release and fail open on a mid-version change.
const slotEnumerateDistributions = 15

const (
	coinitApartmentThreaded = 0x2
	clsctxLocalServer       = 0x4

	// RPC_C_IMP_LEVEL_IMPERSONATE. The service impersonates the caller to act
	// on that user's distros; without it every call returns
	// ERROR_BAD_IMPERSONATION_LEVEL (0x80070542), measured in spike/d.
	rpcImpLevelImpersonate = 3
)

var (
	ole32                    = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoInitializeSecurity = ole32.NewProc("CoInitializeSecurity")
	procCoCreateInstance     = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
)

// lxssErrorInfo mirrors LXSS_ERROR_INFO. The service writes into it; we pass it
// so the call has somewhere to put a message, and read it only on failure.
type lxssErrorInfo struct {
	Flags        uint32
	_            uint32 // padding before the 8-aligned Context
	Context      uint64
	Message      *uint16
	Warnings     *uint16
	WarningsPipe uint32
	_            uint32
}

// lxssEnumerateInfo mirrors LXSS_ENUMERATE_INFO. Size confirmed against the
// running service at 544 bytes (spike/d).
type lxssEnumerateInfo struct {
	DistroGuid windows.GUID
	State      uint32
	Version    uint32
	Flags      uint32
	DistroName [257]uint16
}

// LxssDistributionState values that matter here. The rest are transient states
// during install, convert, export or compact, and none of them mean "running".
const (
	lxssStateRunning = 2
)

// comSession owns an ILxssUserSession on a thread of its own.
//
// The thread is not an optimisation. A COM apartment belongs to an OS THREAD,
// and Go moves goroutines between threads freely, so a call can land on a
// thread that never saw CoInitializeEx and fail with CO_E_NOTINITIALIZED. In
// spike/d that happened in 2 of 4 runs — intermittent, load-dependent, and
// exactly the kind of failure that survives testing and reaches a user as an
// unexplained supervisor error. So every COM call is funnelled to one locked
// thread through a channel.
type comSession struct {
	calls chan func()
	stop  chan struct{}
	once  sync.Once
	sess  unsafe.Pointer

	// callTimeout bounds one call; zero means comCallTimeout. A field rather
	// than a mutable package global so a test can shorten it without other
	// tests in the same binary seeing the change.
	callTimeout time.Duration
}

// newCOMSession brings up the apartment and the object, or reports why it could
// not. A failure here is never fatal: the caller falls back to wsl.exe.
func newCOMSession() (*comSession, error) {
	s := &comSession{calls: make(chan func()), stop: make(chan struct{})}
	ready := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// S_FALSE means this thread was already in an apartment, which is fine.
		procCoInitializeEx.Call(0, coinitApartmentThreaded)
		defer procCoUninitialize.Call()

		// Process-wide and one-shot: a second call returns
		// RPC_E_TOO_LATE, which is not an error for us.
		procCoInitializeSecurity.Call(0, ^uintptr(0), 0, 0, 0, rpcImpLevelImpersonate, 0, 0, 0)

		var sess unsafe.Pointer
		hr, _, _ := procCoCreateInstance.Call(
			uintptr(unsafe.Pointer(&clsidLxssUserSession)), 0, clsctxLocalServer,
			uintptr(unsafe.Pointer(&iidILxssUserSession)), uintptr(unsafe.Pointer(&sess)))
		if hr != 0 {
			ready <- fmt.Errorf("CoCreateInstance(LxssUserSession): %w", hresult(hr))
			return
		}
		s.sess = sess
		ready <- nil

		for {
			select {
			case fn := <-s.calls:
				fn()
			case <-s.stop:
				syscall.SyscallN(vtable(sess)[2], uintptr(sess)) // Release
				return
			}
		}
	}()

	if err := <-ready; err != nil {
		close(s.stop)
		return nil, err
	}
	return s, nil
}

// comCallTimeout bounds one call on the apartment thread (#437).
//
// Every COM caller used to inherit whatever context it was handed, and the
// supervisor hands down its PROCESS-LIFETIME context — which never fires. So
// a wslservice that stopped answering (a service restart, `wsl --update`, a
// wedged VM) parked the health tick forever, and the tick holds the
// reconciler's mutex: every Demand() blocked, so every docker command HUNG
// rather than failing, and the stats file froze so the tray could not even
// show that the supervisor was stuck.
//
// The COM rewrite (#380) made that worse rather than better. Callers used to
// be separate processes shelling out to wsl.exe and could not affect each
// other; now List, Terminate and doctor all queue behind one unbuffered
// channel and one OS thread, so one hung call stalls all of them.
//
// A ceiling well above any healthy call — the measurement that motivated this
// path was ~65 ms — so this only ever fires on a service that has stopped
// answering, and never on one that is merely slow.
const comCallTimeout = 20 * time.Second

// do runs fn on the apartment thread and waits for it, bounded.
func (s *comSession) do(ctx context.Context, fn func()) error {
	// Derived, so the caller's own cancellation still wins when it is sooner.
	// Callers distinguish the two: Fast.List checks the OUTER ctx, so a
	// deadline of ours reads as "the backend is unresponsive" and falls back
	// to wsl.exe, while the caller's own cancellation is passed straight
	// through and does not demote the fast path.
	timeout := s.callTimeout
	if timeout == 0 {
		timeout = comCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan struct{})
	var panicked any
	call := func() {
		// Ordered deliberately. close(done) is registered FIRST so it runs
		// LAST: the recover has already stored its value by the time any
		// waiter is released, which is what makes reading `panicked` after
		// <-done race-free.
		defer close(done)
		defer func() { panicked = recover() }()
		fn()
	}

	select {
	case s.calls <- call:
	case <-s.stop:
		return fmt.Errorf("wsl: COM session is closed")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		if panicked != nil {
			// Recovering keeps the apartment loop alive — without it a panic
			// here takes down the whole supervisor, bridge included. But a
			// recovered panic must not read as success: `list` would return
			// an empty slice and a nil error, and a machine with distros
			// would report having none. unsafe.Slice(arr, count) panics if
			// the service ever returns count > 0 with a nil array, and hr==0
			// is the only thing standing between us and that.
			return fmt.Errorf("wsl: COM call panicked: %v", panicked)
		}
		return nil
	case <-ctx.Done():
		// The call is still running on the apartment thread and keeps writing
		// the variables fn captured. That is safe ONLY because every caller
		// returns without reading them on the error path -- list returns
		// `nil, err` and never touches `out`. Now that this deadline can
		// actually fire, that is a live constraint rather than a theoretical
		// one: a future caller that salvages a partial result after a timeout
		// gets a real data race on a slice header.
		//
		// The abandoned call also keeps the single apartment thread busy, so
		// the next do() waits behind it and times out too. Bounded and
		// degraded beats unbounded.
		return ctx.Err()
	}
}

// Close releases the object and stops the thread.
func (s *comSession) Close() { s.once.Do(func() { close(s.stop) }) }

// list is EnumerateDistributions, shaped as the WSL interface wants it.
func (s *comSession) list(ctx context.Context) ([]Distro, error) {
	var out []Distro
	var callErr error

	if err := s.do(ctx, func() {
		var count uint32
		var arr *lxssEnumerateInfo
		var info lxssErrorInfo

		hr, _, _ := syscall.SyscallN(vtable(s.sess)[slotEnumerateDistributions], uintptr(s.sess),
			uintptr(unsafe.Pointer(&count)),
			uintptr(unsafe.Pointer(&arr)),
			uintptr(unsafe.Pointer(&info)))
		if hr != 0 {
			callErr = enumError(hr, &info)
			return
		}
		defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(arr)))

		// A machine with no distros reports zero, with no error. The wsl.exe
		// path has to recognise the sentence "no installed distributions" to
		// learn the same thing, which is localised and reworded between
		// releases — one of the reasons this path exists.
		if count == 0 {
			return
		}
		for _, d := range unsafe.Slice(arr, count) {
			out = append(out, Distro{
				Name:    windows.UTF16ToString(d.DistroName[:]),
				State:   stateName(d.State),
				Version: int(d.Version),
			})
		}
	}); err != nil {
		return nil, err
	}
	return out, callErr
}

// stateName maps LxssDistributionState onto the strings the CLI prints, so
// callers comparing State see the same values from either backend.
//
// Only Running is named. Everything else is reported as Stopped, matching what
// `wsl --list --verbose` shows for an installed distro that is not up, and
// keeping Distro.Running() — the only thing anything actually asks — correct.
func stateName(s uint32) string {
	if s == lxssStateRunning {
		return "Running"
	}
	return "Stopped"
}

// Default is deliberately not filled in. EnumerateDistributions does not report
// it, GetDefaultDistribution is a second round trip, and nothing in Skrog reads
// Distro.Default — inventing a value that is always false would be worse than
// leaving it at the zero value the CLI path also produces when it cannot tell.

func vtable(obj unsafe.Pointer) *[64]uintptr { return *(**[64]uintptr)(obj) }

// enumError turns a failed call into something a person can act on, preferring
// the service's own message over the bare code.
func enumError(hr uintptr, info *lxssErrorInfo) error {
	if info.Message != nil {
		return fmt.Errorf("EnumerateDistributions: %w: %s", hresult(hr),
			windows.UTF16PtrToString(info.Message))
	}
	return fmt.Errorf("EnumerateDistributions: %w", hresult(hr))
}

// hresult renders an HRESULT as a Windows error where it maps to one, which is
// the point of this backend: real error codes instead of matching on the text
// wsl.exe happened to print in the user's language.
func hresult(hr uintptr) error {
	code := uint32(hr)
	// HRESULT_FROM_WIN32: 0x8007xxxx wraps a Win32 error.
	if code&0xFFFF0000 == 0x80070000 {
		return windows.Errno(code & 0xFFFF)
	}
	return fmt.Errorf("HRESULT 0x%08X", code)
}

// Further vtable slots, from the same wslservice.idl method ordering and
// anchored on the one already proven: EnumerateDistributions is declaration 13,
// three IUnknown methods ahead of it, so slot 15 — which is the constant above,
// verified working against the live service. Counting from that anchor:
//
//	6  GetDistributionId       (declaration  4)
//	7  TerminateDistribution   (declaration  5)
//
// The IID gate described above covers these the same way. A reshaped interface
// is a new IID, so QueryInterface fails and none of this path runs.
const (
	slotGetDistributionID     = 6
	slotTerminateDistribution = 7
)

// distributionID resolves a distro name to the GUID the GUID-taking methods
// need. Callers hold the apartment thread already, so this does no do().
func (s *comSession) distributionIDLocked(name string) (windows.GUID, error) {
	var guid windows.GUID
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return guid, err
	}
	var info lxssErrorInfo
	hr, _, _ := syscall.SyscallN(vtable(s.sess)[slotGetDistributionID], uintptr(s.sess),
		uintptr(unsafe.Pointer(n)),
		0, // Flags
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&guid)))
	if hr != 0 {
		return guid, callError("GetDistributionId", hr, &info)
	}
	return guid, nil
}

// terminate stops a running distro over COM (wsl --terminate).
//
// Two round trips rather than one: the service addresses distros by GUID and
// Skrog knows them by name. Still far cheaper than a spawn, and unlike the CLI
// path the "no such distro" case arrives as an HRESULT rather than as a
// localised sentence to match on.
func (s *comSession) terminate(ctx context.Context, distro string) error {
	var callErr error
	if err := s.do(ctx, func() {
		guid, err := s.distributionIDLocked(distro)
		if err != nil {
			callErr = err
			return
		}
		var info lxssErrorInfo
		hr, _, _ := syscall.SyscallN(vtable(s.sess)[slotTerminateDistribution], uintptr(s.sess),
			uintptr(unsafe.Pointer(&guid)),
			uintptr(unsafe.Pointer(&info)))
		if hr != 0 {
			callErr = callError("TerminateDistribution", hr, &info)
		}
	}); err != nil {
		return err
	}
	return callErr
}

// ServiceError is an error the SERVICE returned, as opposed to a failure of
// the COM plumbing around it.
//
// The distinction decides whether the fast path gets demoted. "There is no
// distribution with the supplied name" is the service answering correctly; a
// caller that treated it as the interface having moved would fall back to
// wsl.exe, run the same doomed operation a second time, and disable COM for
// the rest of the process -- all three of which the live test caught it doing.
//
// The IID gate already covers slot correctness, so a call that came back at
// all is evidence the surface is intact, whatever it came back with.
type ServiceError struct {
	Method string
	Err    error
	Detail string
}

func (e *ServiceError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %v: %s", e.Method, e.Err, e.Detail)
	}
	return fmt.Sprintf("%s: %v", e.Method, e.Err)
}

func (e *ServiceError) Unwrap() error { return e.Err }

// callError is enumError generalised over the method name, now that more than
// one call can fail.
func callError(method string, hr uintptr, info *lxssErrorInfo) error {
	e := &ServiceError{Method: method, Err: hresult(hr)}
	if info.Message != nil {
		e.Detail = windows.UTF16PtrToString(info.Message)
	}
	return e
}
