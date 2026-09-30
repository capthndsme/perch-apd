//go:build noplane

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/handlers"
)

// A build without the Wi-Fi config plane (`go build -tags noplane`): only
// for `make size`, which measures what the plane adds to the binary.

func newWifiPlane(*config.Config, *device, *slog.Logger) daemonPlane { return nil }

func cliWifiPlane(*config.Config, *device) handlers.WifiPlane { return nil }

// configGuardCommand fails, so the guard script restores by itself.
func configGuardCommand(_ []string, _, errw io.Writer, _ string) int {
	fmt.Fprintln(errw, "perch-apd config-guard: this build has no Wi-Fi config plane")
	return 1
}

func runPlaneWifi(context.Context, string, []string) (int, bool) { return 0, false }
