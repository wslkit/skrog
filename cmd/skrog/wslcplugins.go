package main

import (
	"log/slog"

	"github.com/wslkit/skrog/internal/config"
	"github.com/wslkit/skrog/internal/wslc"
)

// refuseIfPluginsPresent stops the wslc backend from serving on a machine
// whose WSL plugins would not fire through the relay (#406).
//
// #322 closed the registry-allowlist half of the docker.sock bypass by
// standing in for the policy — the rules are declarative, so Skrog can read
// the same keys and reach the same verdict. Plugin hooks are not declarative.
// They are third-party DLLs with a veto over container creation, and there is
// no substituting for code we do not have.
//
// The choice is therefore between serving and silently disabling somebody's
// security tooling, or refusing and saying why. This refuses. It is the same
// posture the project already takes for a policy it cannot read (#254).
//
// Reading the plugin list fails CLOSED for the same reason: a key that exists
// but will not enumerate is not an absent key.
func refuseIfPluginsPresent(stateDir string, log *slog.Logger) error {
	plugins, err := wslc.ReadPlugins()
	if err != nil {
		return err
	}
	if !plugins.Any() {
		return nil
	}

	// The override is read AFTER detection so the log line names what is being
	// ignored. "Serving anyway" is a defensible choice; making it without
	// knowing which tool went quiet is not.
	cfg, cfgErr := config.Load(stateDir)
	if cfgErr == nil && cfg.WslcIgnorePlugins {
		if log != nil {
			log.Warn("serving the wslc backend with WSL plugins registered; their hooks will NOT fire",
				"plugins", plugins.Names(), "override", config.KeyWslcIgnorePlugins)
		}
		return nil
	}
	return &wslc.ErrPluginsPresent{Plugins: plugins}
}
