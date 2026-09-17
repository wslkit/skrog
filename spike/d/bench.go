//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"time"
)

// benchCLI times what internal/wsl.Local does today for List: spawn wsl.exe.
func benchCLI(n int) {
	// Warm-up, discarded: the first spawn pays for loading wsl.exe and any
	// service start, which is not what a steady-state poll costs.
	_, _ = exec.Command("wsl.exe", "-l", "-v").Output()

	t0 := time.Now()
	for i := 0; i < n; i++ {
		if _, err := exec.Command("wsl.exe", "-l", "-v").Output(); err != nil {
			// wsl -l -v exits non-zero in some states; the spawn cost is the
			// measurement, so keep going.
			_ = err
		}
	}
	d := time.Since(t0)
	fmt.Printf("\nwsl.exe -l -v: %d calls in %v  (mean %v per call)\n", n, d, d/time.Duration(n))
}
