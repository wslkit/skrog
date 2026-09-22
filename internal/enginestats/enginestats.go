// Package enginestats reads what the engine, its disk and its VM are actually
// doing (#179).
//
// Everything here is gated on the engine already running. `skrog status` must
// never boot a stopped distro (#82) -- that is what makes the idle-stop RAM
// story true -- so a stopped engine yields Probed=false and nothing is started
// to find out more.
//
// The engine is queried from *inside* the distro over its own unix socket,
// through socat (which the rootfs carries for the fallback transport). That
// choice matters: it means statistics work with no docker CLI on the host, no
// dependency on the bridge being up, and no traffic through the pipe that the
// bridge's own counters would then report as activity -- a status command that
// showed itself in the numbers would be its own kind of wrong.
package enginestats

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Distro runs commands inside the engine distro.
type Distro interface {
	Exec(ctx context.Context, distro, user string, args ...string) (string, error)
}

// Disk measures the virtual disk on the host.
type Disk interface {
	// SizeOnDisk is the .vhdx's footprint on the volume.
	SizeOnDisk(path string) (uint64, error)
	// Free is free space on the volume holding the path.
	Free(path string) (uint64, error)
}

// Stats is the whole picture. Probed says whether the engine was up; when it is
// false every group below is zero and means nothing.
type Stats struct {
	Probed bool        `json:"probed"`
	Engine EngineStats `json:"engine"`
	Disk   DiskStats   `json:"disk"`
	VM     VMStats     `json:"vm"`
	Errors []string    `json:"errors,omitempty"`
}

// EngineStats is what the daemon holds.
type EngineStats struct {
	Version         string `json:"version"`
	Containers      int    `json:"containers"`
	Running         int    `json:"containersRunning"`
	Paused          int    `json:"containersPaused"`
	Stopped         int    `json:"containersStopped"`
	Images          int    `json:"images"`
	Volumes         int    `json:"volumes,omitempty"`
	BuildCacheBytes uint64 `json:"buildCacheBytes,omitempty"`
	ImagesBytes     uint64 `json:"imagesBytes,omitempty"`
	VolumesBytes    uint64 `json:"volumesBytes,omitempty"`
	// Reclaimable is what the engine says `docker system prune` could free.
	ReclaimableBytes uint64 `json:"reclaimableBytes,omitempty"`
}

// DiskStats is the host-side footprint, and what compaction could give back.
type DiskStats struct {
	Path string `json:"path"`
	// SizeOnDiskBytes is what the .vhdx occupies on the volume; GuestUsedBytes
	// what the filesystem inside it uses.
	SizeOnDiskBytes uint64 `json:"sizeOnDiskBytes"`
	GuestUsedBytes  uint64 `json:"guestUsedBytes,omitempty"`
	// ReclaimableBytes is the difference: space the file holds that the guest
	// is not using, i.e. roughly what `skrog compact` could return. An
	// estimate, not a promise -- compaction works in blocks, and a block with
	// one live byte in it stays.
	ReclaimableBytes uint64 `json:"reclaimableBytes,omitempty"`
	HostFreeBytes    uint64 `json:"hostFreeBytes,omitempty"`
}

// VMStats is the WSL2 VM's own resources: what it has, versus what
// ~/.wslconfig asked for. The two disagreeing is the trap `skrog wsl-config`
// exists to close -- a limit recorded and never applied looks like a limit.
type VMStats struct {
	CPUs              int    `json:"cpus"`
	MemTotalBytes     uint64 `json:"memTotalBytes"`
	MemAvailableBytes uint64 `json:"memAvailableBytes,omitempty"`
	SwapTotalBytes    uint64 `json:"swapTotalBytes,omitempty"`
	// ConfiguredMemory and ConfiguredProcessors are what ~/.wslconfig sets,
	// empty when it sets nothing (WSL's defaults then apply).
	ConfiguredMemory     string `json:"configuredMemory,omitempty"`
	ConfiguredProcessors string `json:"configuredProcessors,omitempty"`
}

// Reader gathers the statistics.
type Reader struct {
	WSL  Distro
	Disk Disk
	// Configured is the sizing from ~/.wslconfig, keyed by .wslconfig name.
	// Passed in rather than read here so this package stays free of file
	// layout concerns.
	Configured map[string]string
}

// Read gathers everything it can, recording failures rather than aborting: a
// missing `df` should not cost you the container counts. running must be the
// caller's own check -- this package never starts anything to find out.
func (r *Reader) Read(ctx context.Context, distro, diskPath string, running bool) Stats {
	s := Stats{}
	if !running {
		return s
	}
	s.Probed = true

	if info, err := r.dockerInfo(ctx, distro); err != nil {
		s.Errors = append(s.Errors, "engine info: "+err.Error())
	} else {
		s.Engine.Version = info.ServerVersion
		s.Engine.Containers = info.Containers
		s.Engine.Running = info.ContainersRunning
		s.Engine.Paused = info.ContainersPaused
		s.Engine.Stopped = info.ContainersStopped
		s.Engine.Images = info.Images
		s.VM.CPUs = info.NCPU
		s.VM.MemTotalBytes = uint64(info.MemTotal)
	}

	if df, err := r.systemDF(ctx, distro); err != nil {
		// /system/df walks every layer; on a large engine it is the slow part,
		// and losing it is not worth losing the rest.
		s.Errors = append(s.Errors, "engine disk usage: "+err.Error())
	} else {
		s.Engine.Volumes = len(df.Volumes)
		s.Engine.ImagesBytes = df.LayersSizeBytes
		s.Engine.BuildCacheBytes = df.BuildCacheBytes()
		s.Engine.VolumesBytes = df.VolumesBytes()
		s.Engine.ReclaimableBytes = df.ReclaimableBytes()
	}

	s.Disk.Path = diskPath
	if r.Disk != nil && diskPath != "" {
		if n, err := r.Disk.SizeOnDisk(diskPath); err != nil {
			s.Errors = append(s.Errors, "vhdx size: "+err.Error())
		} else {
			s.Disk.SizeOnDiskBytes = n
		}
		if n, err := r.Disk.Free(diskPath); err == nil {
			s.Disk.HostFreeBytes = n
		}
	}
	if used, err := r.guestUsed(ctx, distro); err != nil {
		s.Errors = append(s.Errors, "guest usage: "+err.Error())
	} else {
		s.Disk.GuestUsedBytes = used
		if s.Disk.SizeOnDiskBytes > used {
			s.Disk.ReclaimableBytes = s.Disk.SizeOnDiskBytes - used
		}
	}

	if avail, swap, err := r.guestMemory(ctx, distro); err != nil {
		s.Errors = append(s.Errors, "guest memory: "+err.Error())
	} else {
		s.VM.MemAvailableBytes = avail
		s.VM.SwapTotalBytes = swap
	}
	s.VM.ConfiguredMemory = r.Configured["memory"]
	s.VM.ConfiguredProcessors = r.Configured["processors"]
	return s
}

// dockerInfoResponse is the subset of /info this reads.
type dockerInfoResponse struct {
	ServerVersion     string `json:"ServerVersion"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersPaused  int    `json:"ContainersPaused"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
}

func (r *Reader) dockerInfo(ctx context.Context, distro string) (dockerInfoResponse, error) {
	body, err := r.apiGet(ctx, distro, "/info")
	if err != nil {
		return dockerInfoResponse{}, err
	}
	var out dockerInfoResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return dockerInfoResponse{}, fmt.Errorf("parsing /info: %w", err)
	}
	return out, nil
}

// dfResponse is the subset of /system/df this reads. The engine reports sizes
// per object with a shared-size notion; summing LayersSize double-counts
// nothing because the engine already accounts for sharing there.
type dfResponse struct {
	LayersSizeBytes uint64 `json:"LayersSize"`
	Images          []struct {
		Size       uint64 `json:"Size"`
		Containers int    `json:"Containers"`
	} `json:"Images"`
	Volumes []struct {
		UsageData struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		Size        uint64 `json:"Size"`
		InUse       bool   `json:"InUse"`
		Shared      bool   `json:"Shared"`
		Description string `json:"Description"`
	} `json:"BuildCache"`
}

func (d dfResponse) BuildCacheBytes() uint64 {
	var n uint64
	for _, c := range d.BuildCache {
		if !c.Shared {
			n += c.Size
		}
	}
	return n
}

func (d dfResponse) VolumesBytes() uint64 {
	var n uint64
	for _, v := range d.Volumes {
		if v.UsageData.Size > 0 {
			n += uint64(v.UsageData.Size)
		}
	}
	return n
}

// ReclaimableBytes is what a prune could free: images no container uses,
// unreferenced volumes, and build cache that is not in use.
func (d dfResponse) ReclaimableBytes() uint64 {
	var n uint64
	for _, i := range d.Images {
		if i.Containers == 0 {
			n += i.Size
		}
	}
	for _, v := range d.Volumes {
		if v.UsageData.RefCount == 0 && v.UsageData.Size > 0 {
			n += uint64(v.UsageData.Size)
		}
	}
	for _, c := range d.BuildCache {
		if !c.InUse && !c.Shared {
			n += c.Size
		}
	}
	return n
}

func (r *Reader) systemDF(ctx context.Context, distro string) (dfResponse, error) {
	body, err := r.apiGet(ctx, distro, "/system/df")
	if err != nil {
		return dfResponse{}, err
	}
	var out dfResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return dfResponse{}, fmt.Errorf("parsing /system/df: %w", err)
	}
	return out, nil
}

// apiGet performs one GET against the engine's unix socket from inside the
// distro, using socat, and returns the response body.
//
// HTTP/1.0 with no keep-alive: the response ends at EOF, so there is no
// Content-Length or chunked framing to parse in a shell pipeline.
//
// shut-none is load-bearing, and was found the hard way. Without it socat
// propagates printf's EOF as a half-close on the socket, dockerd reads that as
// the client going away, and cancels the request mid-flight: /info answers a
// literal `null` — which unmarshals into zeroes and reads as an empty engine —
// and /system/df answers 499. With shut-none the socket stays open until
// dockerd closes it after the response, which for HTTP/1.0 is immediately, so
// the fix costs nothing (measured at 0s).
func (r *Reader) apiGet(ctx context.Context, distro, path string) ([]byte, error) {
	cmd := fmt.Sprintf(`printf 'GET %s HTTP/1.0\r\nHost: docker\r\n\r\n' | `+
		`socat -t 5 - UNIX-CONNECT:/var/run/docker.sock,shut-none`, path)
	out, err := r.WSL.Exec(ctx, distro, "root", "sh", "-c", cmd)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", path, err)
	}
	body, err := httpBody(out)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", path, err)
	}
	// Defence in depth against the cancelled-request answer: a literal `null`
	// unmarshals into a zeroed struct, which would read as "no containers, no
	// images, no memory" -- a wrong answer is worse than no answer. shut-none
	// above stops this happening; refusing it here stops it ever being
	// believed if it comes back another way.
	if s := strings.TrimSpace(string(body)); s == "" || s == "null" {
		return nil, fmt.Errorf("querying %s: the engine returned an empty body "+
			"(the request was probably cancelled)", path)
	}
	return body, nil
}

// HTTPBody is httpBody for the other in-distro readers of the engine socket
// (`skrog top`, #511), so the status check is written once.
func HTTPBody(raw string) ([]byte, error) { return httpBody(raw) }

// httpBody splits a raw HTTP response, checking the status line: a 404 with a
// JSON body would otherwise unmarshal into zeroes and read as an empty engine.
func httpBody(raw string) ([]byte, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	head, body, ok := strings.Cut(raw, "\n\n")
	if !ok {
		return nil, fmt.Errorf("no HTTP body in %d bytes of response", len(raw))
	}
	status := head
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		status = head[:i]
	}
	f := strings.Fields(status)
	if len(f) < 2 {
		return nil, fmt.Errorf("unparseable status line %q", status)
	}
	code, err := strconv.Atoi(f[1])
	if err != nil {
		return nil, fmt.Errorf("unparseable status code in %q", status)
	}
	if code < 200 || code > 299 {
		return nil, fmt.Errorf("engine returned %s", strings.TrimSpace(status))
	}
	return []byte(strings.TrimSpace(body)), nil
}

// guestUsed is how much of the engine's filesystem is in use, in bytes -- the
// other half of the reclaimable estimate.
func (r *Reader) guestUsed(ctx context.Context, distro string) (uint64, error) {
	// -B1 for bytes rather than blocks; POSIX output so the columns are fixed.
	out, err := r.WSL.Exec(ctx, distro, "root", "sh", "-c", "df -B1 -P / | tail -1")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) < 3 {
		return 0, fmt.Errorf("unparseable df output %q", strings.TrimSpace(out))
	}
	return strconv.ParseUint(f[2], 10, 64)
}

// guestMemory reads available memory and swap from /proc/meminfo. MemAvailable
// is the kernel's own estimate of what a workload could claim without
// swapping, which is the number worth showing -- "free" on a machine with page
// cache is misleading.
func (r *Reader) guestMemory(ctx context.Context, distro string) (avail, swap uint64, err error) {
	out, err := r.WSL.Exec(ctx, distro, "root", "sh", "-c",
		"grep -E '^(MemAvailable|SwapTotal):' /proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, perr := strconv.ParseUint(f[1], 10, 64)
		if perr != nil {
			continue
		}
		switch strings.TrimSuffix(f[0], ":") {
		case "MemAvailable":
			avail = kb * 1024
		case "SwapTotal":
			swap = kb * 1024
		}
	}
	return avail, swap, nil
}
