// Package version holds the build version, set at link time:
//
//	go build -ldflags "-X github.com/capthndsme/perch-apd/internal/version.Version=0.1.0"
package version

import "runtime"

// Version is the daemon version ("dev" for unreleased builds).
var Version = "dev"

// Arch is the release name of the build's architecture: the file suffix of
// the release asset (amd64, arm64, armv7, mipsle, mips, …).
func Arch() string {
	switch runtime.GOARCH {
	case "arm":
		return "arm" + goarm
	default:
		return runtime.GOARCH
	}
}

// UserAgent is sent with every request to the controller.
func UserAgent() string {
	return "perch-apd/" + Version + " (" + runtime.GOOS + "/" + Arch() + ")"
}
