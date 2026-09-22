//go:build windows

package vmtop

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReadVmmem finds the WSL VM's memory process and reads it.
//
// From the system process list (NtQuerySystemInformation), not by opening the
// process: vmmem refuses OpenProcess to an unelevated caller, even with
// PROCESS_QUERY_LIMITED_INFORMATION (measured, "Access is denied"), while the
// process list carries the same counters for every process. It is also where
// Task Manager and Get-Process read them.
//
// vmmemWSL is unambiguous. Plain vmmem is WSL's on Windows 10 but is also the
// name every Hyper-V VM's memory process has, so with more than one of them
// and no vmmemWSL the answer is refused rather than guessed: a number from
// the wrong VM is worse than none.
func ReadVmmem() (*Vmmem, error) {
	buf, err := processList()
	if err != nil {
		return nil, err
	}

	var wsl *Vmmem
	var plain []*Vmmem
	for off := uintptr(0); off < uintptr(len(buf)); {
		p := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
		name := strings.TrimSuffix(strings.ToLower(p.ImageName.String()), ".exe")
		if name == "vmmemwsl" || name == "vmmem" {
			v := &Vmmem{
				Process:                name,
				PID:                    uint32(p.UniqueProcessID),
				WorkingSetBytes:        uint64(p.WorkingSetSize),
				PrivateWorkingSetBytes: uint64(max(p.WorkingSetPrivateSize, 0)),
				PrivateBytes:           uint64(p.PrivatePageCount),
			}
			if name == "vmmemwsl" {
				v.Process = "vmmemWSL"
				wsl = v
			} else {
				plain = append(plain, v)
			}
		}
		if p.NextEntryOffset == 0 {
			break
		}
		off += uintptr(p.NextEntryOffset)
	}

	switch {
	case wsl != nil:
		return wsl, nil
	case len(plain) == 1:
		return plain[0], nil
	case len(plain) > 1:
		return nil, fmt.Errorf("%d vmmem processes and no vmmemWSL: cannot tell which VM is WSL's", len(plain))
	default:
		return nil, errors.New("no vmmem process: the WSL VM is not running")
	}
}

// processList returns the raw SystemProcessInformation buffer, growing it
// until the list fits: processes start between the size query and the read.
func processList() ([]byte, error) {
	size := uint32(512 << 10)
	for range 8 {
		buf := make([]byte, size)
		var need uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation,
			unsafe.Pointer(&buf[0]), size, &need)
		if err == nil {
			return buf, nil
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			return nil, fmt.Errorf("listing processes: %w", err)
		}
		size = max(need+64<<10, size*2)
	}
	return nil, errors.New("listing processes: the list kept growing")
}
