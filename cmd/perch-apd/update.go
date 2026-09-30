package main

import (
	"io"
	"log/slog"

	"github.com/capthndsme/perch-apd/internal/agent"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/update"
)

// newUpdater builds agent self-update (internal/update) for the daemon: it
// shares the device's write lock with device groups and the Wi-Fi plane.
// nil (logged) when it cannot be built; the daemon runs on without it.
func newUpdater(cfg *config.Config, d *device, log *slog.Logger) *update.Updater {
	client, err := agent.NewHTTPClient(cfg)
	if err != nil {
		log.Error("self-update is off: no HTTP client", "err", err)
		return nil
	}
	u, err := update.New(update.Options{Config: cfg, Lock: d.applyLock, HTTP: client, Log: log})
	if err != nil {
		log.Error("self-update is off", "err", err)
		return nil
	}
	if !cfg.SelfUpdate {
		log.Info("self-update is off (option self_update '0'): the controller cannot update this AP")
	}
	return u
}

// cliUpdater is the updater `perch-apd info` shows: read only, nothing is
// resumed or locked.
func cliUpdater(cfg *config.Config) *update.Updater {
	u, err := update.New(update.Options{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return nil
	}
	return u
}
