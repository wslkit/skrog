package vmtop

// Vmmem is what Windows says the VM's memory process holds (#511).
//
// Three numbers, because they answer different questions and none of them is
// the guest's own view:
//
//   - WorkingSetBytes is the physical memory Windows currently has mapped for
//     the process;
//   - PrivateWorkingSetBytes is the part of that no other process shares --
//     the "Memory" column in Task Manager's Processes tab;
//   - PrivateBytes is what Windows has committed on the VM's behalf, resident
//     or not.
type Vmmem struct {
	// Process is the image name: vmmemWSL on Windows 11, vmmem on Windows 10.
	Process                string `json:"process"`
	PID                    uint32 `json:"pid"`
	WorkingSetBytes        uint64 `json:"workingSetBytes"`
	PrivateWorkingSetBytes uint64 `json:"privateWorkingSetBytes"`
	PrivateBytes           uint64 `json:"privateBytes"`
}
