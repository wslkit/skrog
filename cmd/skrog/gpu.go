package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/gpu"
	"github.com/wslkit/skrog/internal/provision"
)

// runEnableGPU is `skrog enable-gpu`: install the NVIDIA CDI spec so containers
// can use the GPU (#83). WSL2 already projects the driver into the distro; this
// tells the engine to inject it into containers.
func runEnableGPU(args []string) int {
	fs := flag.NewFlagSet("enable-gpu", flag.ContinueOnError)
	var (
		stateDir = fs.String("state-dir", "", "override Skrog's state directory")
		distro   = fs.String("distro", "", "WSL distro (default: from the install manifest)")
		off      = fs.Bool("off", false, "disable GPU access (remove the CDI spec)")
		vendor   = fs.String("vendor", "", "GPU vendor: nvidia (default) or amd (EXPERIMENTAL, untested on hardware)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: skrog enable-gpu [--off]

Enables NVIDIA GPU access for containers, then a container reaches the GPU with:

  docker run --rm --device nvidia.com/gpu=all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi

WSL2 already projects the Windows NVIDIA driver into the engine distro
(/usr/lib/wsl/lib, /dev/dxg); this installs a Container Device Interface (CDI)
spec so dockerd injects that into containers. No toolkit is installed in the
distro, and it works with the musl-based engine because the spec is hookless
(it sets LD_LIBRARY_PATH rather than running an ldconfig hook).

The setting persists: the spec is re-installed on every engine start, so a
reinstall keeps GPU access. --off removes it.

Requires an NVIDIA GPU with a WSL-capable driver. Intel GPUs are not wired up.

  --vendor amd   EXPERIMENTAL (#185), and never run against real hardware.

AMD reaches the GPU the same way -- ROCm's ROCDXG talks to the Windows driver
over the same /dev/dxg, and libdxcore.so is projected into the same
/usr/lib/wsl/lib -- so the spec is this one with kind amd.com/gpu. It is
written from AMD's documentation, not from observation, so it is opt-in by
name and reports itself as experimental. Needs AMD Software: Adrenalin Edition
26.2.2 for WSL2 or newer, and a ROCm container image (ROCm is Ubuntu-only, so
the image must be glibc; the engine's own musl is irrelevant because nothing
is installed in it). JAX, MIGraphX and multi-GPU are unsupported under WSL by
AMD, not by Skrog.

Note the vendor is taken at your word: the only library AMD projects here is
libdxcore.so, which is Microsoft's DXCore shim and is present on NVIDIA
machines too, so there is nothing to auto-detect it with.

Exit codes: 0 ok, %d error, %d usage, %d not installed / no GPU.

flags:
`, exitError, exitUsage, exitNotFound)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	v, err := gpu.ParseVendor(*vendor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitUsage
	}

	opts := optsWithResolvedStateDir(provision.Options{StateDir: *stateDir, Distro: *distro, GPUVendor: string(v)})
	log := cliLogger(false)
	p := &provision.Provisioner{Logger: log}

	targetDistro, ok := requireDistroInstall(p, opts)
	if !ok {
		return exitNotFound
	}
	opts.Distro = targetDistro

	ctx, stop := interruptible()
	defer stop()

	if *off {
		if err := config.Set(opts.StateDir, config.KeyGPU, "off"); err != nil {
			fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
			return exitError
		}
		// dockerd reads CDI specs dynamically, so removing the file takes effect
		// on the next `docker run` — no restart needed. The config flag keeps a
		// fresh engine start (a reinstall) from re-adding it.
		if err := p.ConfigureGPU(ctx, opts, false); err != nil {
			log.Warn("could not remove the CDI spec", "error", err)
		}
		fmt.Println("GPU access disabled.")
		return exitOK
	}

	// The distro must actually see the GPU, or the CDI spec would inject devices
	// that are not there. This is the honest gate for "is there an NVIDIA GPU
	// with a WSL driver".
	if !p.GPUAvailable(ctx, opts) {
		fmt.Fprintf(os.Stderr, `skrog: no %s GPU is visible to WSL in this distro.

Checked for %s and %s inside %q and did not find them. That means one of:
  - this machine has no %s GPU with WSL support;
  - the Windows driver is too old for WSL GPU support — update it;
  - WSL itself is out of date — run `+"`wsl --update`"+`.
`, v, gpu.DxgDevice, v.ProbeLib(), opts.Distro, v)
		return exitNotFound
	}

	if err := config.Set(opts.StateDir, config.KeyGPUVendor, string(v)); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	if err := config.Set(opts.StateDir, config.KeyGPU, "on"); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: %v\n", err)
		return exitError
	}
	// Write the spec into the running distro; dockerd picks up CDI specs
	// dynamically, so the GPU is usable on the next `docker run` with no restart.
	// The config flag makes a fresh engine start (after a reinstall) re-apply it.
	if err := p.ConfigureGPU(ctx, opts, true); err != nil {
		fmt.Fprintf(os.Stderr, "skrog: installing the CDI spec: %v\n", err)
		return exitError
	}

	// AMD gets its own closing message: none of the NVIDIA advice below
	// applies (no nvidia-smi, no nvidia-cdi-hook, and the ROCm userspace has
	// to come from the image), and the experimental status has to be said
	// where someone will actually read it.
	if v == gpu.AMD {
		fmt.Printf(`GPU access enabled for AMD — EXPERIMENTAL, and untested on real hardware.

  docker run --rm --device amd.com/gpu=all rocm/rocm-terminal rocm-smi

This spec is written from AMD's ROCm-on-WSL documentation, not from a machine
anyone has run it on (#185). If it works, or does not, please say so on that
issue — that is the only way it stops being experimental.

What it needs:
  - AMD Software: Adrenalin Edition 26.2.2 for WSL2 or newer on Windows;
  - a ROCm container image. ROCm is Ubuntu-only, so the image must be glibc;
    the engine's own musl does not matter, since nothing is installed in it.

Known AMD limits under WSL, not Skrog's: no multi-GPU, no MIGraphX, and the
ROCm build of JAX is not validated.

` + "`--gpus all`" + ` is NOT routed here — that path is NVIDIA-specific in moby. Use
` + "`--device amd.com/gpu=all`" + `.
`)
		return exitOK
	}

	// Which spelling works depends on the rootfs: `--gpus all` needs
	// nvidia-cdi-hook present when dockerd starts (rootfs 29.7.2-4+, #139);
	// `--device nvidia.com/gpu=all` works on every rootfs.
	if p.GPUHookInstalled(ctx, opts) {
		fmt.Printf(`GPU access enabled. Run a container against it:

  docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi

` + "`--gpus all`" + ` (and VS Code Dev Containers' "hostRequirements": {"gpu": true}) route
to the CDI spec; ` + "`--device nvidia.com/gpu=all`" + ` works as well. If the engine was
already running before this rootfs gained nvidia-cdi-hook, ` + "`skrog restart`" + ` once.

Prefer glibc CUDA base images (nvidia/cuda, ubuntu); the driver libraries are
injected via LD_LIBRARY_PATH, which musl images honor too, but nvidia/cuda is
the best-tested path.
`)
		return exitOK
	}
	fmt.Printf(`GPU access enabled. Run a container against it:

  docker run --rm --device nvidia.com/gpu=all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi

(Compose: add a device with driver "cdi" and id "nvidia.com/gpu=all".)

This rootfs predates nvidia-cdi-hook, so ` + "`docker run --gpus all`" + ` is not routed
to the GPU here; a rootfs of 29.7.2-4 or later adds it (reinstall to pick it up).

Prefer glibc CUDA base images (nvidia/cuda, ubuntu); the driver libraries are
injected via LD_LIBRARY_PATH, which musl images honor too, but nvidia/cuda is
the best-tested path.
`)
	return exitOK
}
