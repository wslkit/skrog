package vmtop

import (
	"os"
	"strings"
	"testing"
	"time"
)

const (
	idAny   = "aa5eb0ef4fcc1c0e4acacea400e16af7978c288306ec2dcb4527960dfd0f902d"
	idWrong = "fd5de946bab2e659bd397c3df3ce0069f3aedc02b95a38705a27ce0a0d4d2974"
)

// testdata/sample.txt is the script's real output on skrog-engine
// (kernel 6.18.40.1-microsoft-standard-WSL2), 2026-09-22: two running
// python:3-alpine containers, three dead containers' groups left behind, and
// no other distro running.
func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestComputeAttributesTheVMFromARealSample(t *testing.T) {
	s := ParseSample(fixture(t))
	if len(s.errors) != 0 {
		t.Fatalf("parse errors: %v", s.errors)
	}
	snap := Compute(s, s)

	if snap.VM.MemTotalBytes != 7980292*1024 || snap.VM.MemUsedBytes != (7980292-7365864)*1024 {
		t.Errorf("VM memory total=%d used=%d", snap.VM.MemTotalBytes, snap.VM.MemUsedBytes)
	}
	if want := uint64(5112+177760) * 1024; snap.VM.PageCacheBytes != want {
		t.Errorf("page cache = %d, want Buffers+Cached = %d", snap.VM.PageCacheBytes, want)
	}
	if snap.VM.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4", snap.VM.CPUs)
	}

	// Only this engine's RUNNING containers, by name; the dead groups are not
	// containers and hold nothing, so they are nowhere.
	if len(snap.Containers) != 2 {
		t.Fatalf("containers = %+v, want the 2 running ones", snap.Containers)
	}
	byName := map[string]Group{}
	for _, c := range snap.Containers {
		byName[c.Name] = c
	}
	if g := byName["skrog-t-any"]; g.ID != idAny || g.MemoryBytes != 15548416 || g.AnonBytes != 15097856 {
		t.Errorf("skrog-t-any = %+v", g)
	}
	if g := byName["skrog-t-wrong"]; g.ID != idWrong || g.MemoryBytes != 24756224 {
		t.Errorf("skrog-t-wrong = %+v", g)
	}
	if snap.Containers[0].Name != "skrog-t-wrong" {
		t.Errorf("containers not heaviest first: %s first", snap.Containers[0].Name)
	}
	if snap.OtherContainers.MemoryBytes != 0 {
		t.Errorf("dead, empty groups counted as another engine's: %+v", snap.OtherContainers)
	}

	// The engine distro is found by the reader's own cgroup, not by a name.
	if snap.Engine.MemoryBytes != 222466048 {
		t.Errorf("engine = %d, want the wsl-user/distro-3271 group", snap.Engine.MemoryBytes)
	}
	if snap.WSL.MemoryBytes != 921600 {
		t.Errorf("wsl = %d, want wsl-user/non-distro", snap.WSL.MemoryBytes)
	}
	if snap.OtherDistroCount != 0 || snap.OtherDistros.MemoryBytes != 0 {
		t.Errorf("other distros = %d / %d bytes, want none", snap.OtherDistroCount, snap.OtherDistros.MemoryBytes)
	}

	// Uncharged is used minus the TOP-LEVEL groups only; adding the nested
	// ones as well would count every container twice.
	if want := snap.VM.MemUsedBytes - (42471424 + 233943040); snap.UnchargedBytes != want {
		t.Errorf("uncharged = %d, want %d", snap.UnchargedBytes, want)
	}

	// One reading has no CPU window, and says zero rather than inventing one.
	if snap.VM.CPUPercent != 0 || snap.Containers[0].CPUPercent != 0 {
		t.Error("CPU from a single sample should be zero")
	}
}

// CPU is the usage delta over the wall-clock delta, in percent of one CPU,
// the way `docker stats` reports it.
func TestComputeCPUIsTheDeltaOverTheWindow(t *testing.T) {
	text := fixture(t)
	a := ParseSample(text)
	// 1.5 CPU-seconds more for skrog-t-any over a 1 s window: 150%.
	b := ParseSample(strings.Replace(text, "usage_usec 1072885", "usage_usec 2572885", 1))
	a.At = time.Unix(1000, 0)
	b.At = a.At.Add(time.Second)

	snap := Compute(a, b)
	for _, c := range snap.Containers {
		switch c.Name {
		case "skrog-t-any":
			if c.CPUPercent != 150 {
				t.Errorf("skrog-t-any CPU = %v, want 150", c.CPUPercent)
			}
		default:
			if c.CPUPercent != 0 {
				t.Errorf("%s CPU = %v, want 0", c.Name, c.CPUPercent)
			}
		}
	}
	if snap.WindowSecs != 1 {
		t.Errorf("window = %v, want 1", snap.WindowSecs)
	}
}

// Another running distro appears as its own wsl-user group. Its name is not
// visible from inside the VM, so it is counted, not named.
func TestComputeCountsOtherDistros(t *testing.T) {
	text := strings.Replace(fixture(t), "==api",
		"==cg wsl-user/distro-4100/\nmemory.current 300000000\nanon 200000000\nfile 90000000\nkernel 10000000\n==api", 1)
	snap := Compute(ParseSample(text), ParseSample(text))
	if snap.OtherDistroCount != 1 || snap.OtherDistros.MemoryBytes != 300000000 {
		t.Errorf("other distros = %d / %d bytes, want 1 / 300000000",
			snap.OtherDistroCount, snap.OtherDistros.MemoryBytes)
	}
	if snap.Engine.MemoryBytes != 222466048 {
		t.Errorf("the other distro leaked into the engine row: %d", snap.Engine.MemoryBytes)
	}
}

// A container group this engine does not list but that holds memory is
// another engine's (Docker Desktop's, sharing the VM), and is shown as such
// rather than dropped.
func TestComputeKeepsAnotherEnginesContainers(t *testing.T) {
	text := strings.Replace(fixture(t),
		"==cg docker/3fd324679410832744b4d25ce56af4dc4057399070c80ba612fc1c65afcb0026/\nmemory.current 0",
		"==cg docker/3fd324679410832744b4d25ce56af4dc4057399070c80ba612fc1c65afcb0026/\nmemory.current 5000000", 1)
	snap := Compute(ParseSample(text), ParseSample(text))
	if snap.OtherContainers.MemoryBytes != 5000000 {
		t.Errorf("other containers = %d, want 5000000", snap.OtherContainers.MemoryBytes)
	}
	if len(snap.Containers) != 2 {
		t.Errorf("an unlisted group became one of this engine's containers: %+v", snap.Containers)
	}
}

// Without the engine's answer the containers cannot be named, and that is
// reported, not papered over with IDs that look like a complete list.
func TestParseSampleReportsAMissingContainerList(t *testing.T) {
	text := fixture(t)
	text = text[:strings.Index(text, "==api")] + "==api\n"
	s := ParseSample(text)
	if len(s.errors) == 0 {
		t.Error("a missing engine answer produced no error")
	}
	if snap := Compute(s, s); len(snap.Containers) != 0 {
		t.Errorf("containers = %+v with no engine answer, want none", snap.Containers)
	}
}

func TestParseGroupReadsIOAndPressure(t *testing.T) {
	g := parseGroup("memory.current 10\nio 8:48 rbytes=100 wbytes=2408448 rios=0 wios=83\n" +
		"io 8:32 rbytes=5 wbytes=5\npsi.memory avg10=12.50 avg60=0.00 avg300=0.00 total=0\n")
	if g.ioR != 105 || g.ioW != 2408453 {
		t.Errorf("io = %d/%d, want summed across devices", g.ioR, g.ioW)
	}
	if g.psi.Memory != 12.5 {
		t.Errorf("memory pressure = %v, want 12.5", g.psi.Memory)
	}
}
