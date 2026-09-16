//go:build windows

// Command skrogtray is the Skrog status-light tray (PLAN §03, #44): a fixed,
// tiny menu whose every action shells out to the `skrog` CLI. It is built as
// a GUI-subsystem binary (-H=windowsgui) so launching it flashes no console.
//
// The tray deliberately holds no engine logic. It polls `skrog status
// --json` to colour a dot, and each menu item runs a CLI verb; anything that
// would need a decision belongs in the CLI, which is tested. The menu never
// grows past its six groups — that cap is the scope moat.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"fyne.io/systray"
	"github.com/wslkit/skrog/internal/selfexe"
	"github.com/wslkit/skrog/internal/tray"
)

func main() {
	systray.Run(onReady, func() {})
}

// skrogExe resolves the sibling skrog.exe next to this binary, falling back
// to PATH. The tray ships beside the CLI in the release zip.
func skrogExe() string {
	if self, err := selfexe.Path(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "skrog.exe")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "skrog"
}

func onReady() {
	cli := tray.CLI{Exe: skrogExe()}

	systray.SetTitle("Skrog")
	systray.SetIcon(iconGrey)

	// A disabled header shows the state in words; the icon is the dot.
	header := systray.AddMenuItem("Engine: …", "")
	header.Disable()
	systray.AddSeparator()

	// Lifecycle: the three CLI verbs, as data from the logic package.
	lifecycle := make([]*systray.MenuItem, len(tray.Actions))
	for i, a := range tray.Actions {
		lifecycle[i] = systray.AddMenuItem(a.Label, "")
	}
	systray.AddSeparator()

	openLogs := systray.AddMenuItem("Open logs", "Open the supervisor log")
	doctor := systray.AddMenuItem("Run doctor (v0.3)", "Diagnostics arrive in v0.3")
	doctor.Disable() // honest stub: `skrog doctor` is not built yet
	updates := systray.AddMenuItem("Check for updates", "Check skrog, the engine and the docker CLI")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit Skrog tray", "Close this tray (the engine keeps running)")

	// Wire each item to a CLI call in its own goroutine so the click returns
	// immediately and a slow start does not freeze the menu.
	for i, item := range lifecycle {
		go func(item *systray.MenuItem, a tray.Action) {
			for range item.ClickedCh {
				go cli.Run(context.Background(), a)
			}
		}(item, tray.Actions[i])
	}
	go func() {
		for range openLogs.ClickedCh {
			go openLog(cli)
		}
	}()
	go func() {
		for range updates.ClickedCh {
			go checkUpdates(cli, updates)
		}
	}()
	go func() {
		<-quit.ClickedCh
		systray.Quit()
	}()

	// The status loop (#192). Refreshing the dot used to spawn
	// `skrog status --json` every 4s: 285 ms of work every 4000 ms, about 7%
	// of a core for as long as anyone is logged in, to recompute a state the
	// supervisor already probes every tick and writes down.
	//
	// So: learn the state dir once from the CLI, then read the published
	// reading. The expensive call stays as the fallback for when there is no
	// supervisor to publish -- rate-limited, because that is the path that
	// costs a process spawn.
	go func() {
		const (
			paintInterval    = 2 * time.Second
			fallbackInterval = 30 * time.Second
		)
		var (
			stateDir     string
			lastFallback time.Time
		)

		// One CLI call at startup: the initial dot, and the state dir.
		fallback := func() tray.State {
			lastFallback = time.Now()
			st := cli.Poll(context.Background())
			if st.StateDir != "" {
				stateDir = st.StateDir
			}
			return st.State()
		}

		paint := func(s tray.State) {
			header.SetTitle("Engine: " + label(s))
			systray.SetTooltip(tray.Tooltip(s))
			systray.SetIcon(iconFor(s))
		}
		paint(fallback())

		t := time.NewTicker(paintInterval)
		defer t.Stop()
		for range t.C {
			if s, ok := tray.PollPublished(stateDir); ok {
				paint(s)
				continue
			}
			// No fresh published reading: no supervisor, or one too old to
			// trust. Ask the CLI, but not on every tick.
			if time.Since(lastFallback) >= fallbackInterval {
				paint(fallback())
			}
		}
	}()
}

func iconFor(s tray.State) []byte {
	switch {
	case tray.Healthy(s):
		return iconGreen
	case s == tray.StateStopped:
		return iconGrey
	default:
		return iconRed
	}
}

func label(s tray.State) string {
	switch s {
	case tray.StateRunning:
		return "running"
	case tray.StateIdle:
		return "idle (starts on demand)"
	case tray.StateStopped:
		return "stopped"
	case tray.StateNotInstalled:
		return "not installed"
	default:
		return "unknown"
	}
}

func openLog(cli tray.CLI) {
	// The supervisor writes supervisor.log under the default state dir.
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return
	}
	browse(filepath.Join(base, "Skrog", "supervisor.log"))
}

func browse(target string) {
	// rundll32 avoids a shell and handles both URLs and file paths.
	exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
}

// releasesPage is where an upgrade is actually obtained. The tray only ever
// sends people here; it never replaces a binary (#191).
const releasesPage = "https://github.com/wslkit/skrog/releases"

// checkUpdates makes the "Check for updates" item do what it says.
//
// It used to open the releases page and check nothing — a menu item named for
// an action it did not perform. Now it asks the CLI, puts the answer in the
// item's tooltip, and opens the page only when there is something to get.
// Being told "everything is up to date" without a browser window is the
// common case, and the better one.
func checkUpdates(cli tray.CLI, item *systray.MenuItem) {
	item.SetTooltip("Checking…")

	up, err := cli.CheckUpgrades(context.Background())
	if err != nil {
		// The check failed, not the product. Fall back to what the item did
		// before rather than leaving the click with no effect at all.
		item.SetTooltip("Could not check — opening the releases page")
		browse(releasesPage)
		return
	}

	item.SetTooltip(up.Summary)
	if up.Available {
		browse(releasesPage)
	}
}
