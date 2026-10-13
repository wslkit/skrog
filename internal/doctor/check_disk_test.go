package doctor

import (
	"strings"
	"testing"
)

func TestCheckDisk(t *testing.T) {
	c := checkDisk()
	cases := []struct {
		name string
		disk DiskInfo
		want Status
	}{
		{"unavailable", DiskInfo{Err: "windows only"}, Skip},
		{"critically low", DiskInfo{FreeBytes: 1 << 30, TotalBytes: 100 << 30}, Fail},
		{"low", DiskInfo{FreeBytes: 4 << 30, TotalBytes: 100 << 30}, Warn},
		{"plenty", DiskInfo{FreeBytes: 50 << 30, TotalBytes: 100 << 30}, OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Run(Facts{Disk: tc.disk}).Status; got != tc.want {
				t.Fatalf("status = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		512:           "512 B",
		2 * 1024:      "2.0 KiB",
		5 << 30:       "5.0 GiB",
		3 * (1 << 40): "3.0 TiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// With several local engines the floor is judged against the fullest volume any
// of them lives on (#539): the engine being examined has room, another engine
// is on a nearly full drive, and the answer is still a warning that names it.
func TestCheckDiskJudgesEveryEnginesVolume(t *testing.T) {
	c := checkDisk()
	roomy := DiskInfo{Path: `C:\a\distro`, FreeBytes: 50 << 30, TotalBytes: 100 << 30}
	tight := DiskInfo{Path: `D:\b\distro`, FreeBytes: 3 << 30, TotalBytes: 100 << 30}

	r := c.Run(Facts{Disk: roomy, OtherEngines: []EngineDisk{{Name: "proj-a", UsedBytes: 8 << 30, Disk: tight}}})
	if r.Status != Warn {
		t.Fatalf("status = %v, want Warn: another engine's drive is below the floor", r.Status)
	}
	if !strings.Contains(strings.Join(r.Detail, "\n"), "proj-a") || !strings.Contains(r.Remedy, "skrog target remove") {
		t.Errorf("the warning must name the engine and the way out:\n%v\n%s", r.Detail, r.Remedy)
	}

	// Plenty everywhere is still fine, and a machine with one engine reads as before.
	if got := c.Run(Facts{Disk: roomy, OtherEngines: []EngineDisk{{Name: "x", Disk: roomy}}}).Status; got != OK {
		t.Errorf("roomy engines: status = %v, want OK", got)
	}
	if r := c.Run(Facts{Disk: roomy}); r.Status != OK || len(r.Detail) != 0 {
		t.Errorf("a single engine's report changed: %v %v", r.Status, r.Detail)
	}
}
