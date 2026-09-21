// Package ubus calls OpenWrt's ubus through its command-line client. The
// agent only needs a handful of calls (network.wireless status, system
// board, hostapd del_client), none of them on a hot path, so a fork per call
// is cheaper than a native client in binary size and maintenance.
package ubus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrNotFound: the object or method does not exist (ubus exit status 4).
var ErrNotFound = errors.New("ubus: not found")

// ErrUnavailable: there is no ubus on this system.
var ErrUnavailable = errors.New("ubus: not available")

// Runner executes a command and returns stdout; tests replace it.
type Runner func(ctx context.Context, name string, args ...string) (stdout []byte, stderr []byte, exitCode int, err error)

// Client calls ubus.
type Client struct {
	Bin      string
	Timeout  time.Duration
	Run      Runner
	LookPath func(file string) (string, error)
}

// New returns a client for the ubus binary on PATH.
func New() *Client {
	return &Client{Bin: "ubus", Timeout: 5 * time.Second, Run: execRunner, LookPath: exec.LookPath}
}

// Available reports whether the ubus client exists.
func (c *Client) Available() bool {
	if c.Run == nil {
		return false
	}
	lookPath := c.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	_, err := lookPath(c.Bin)
	return err == nil
}

// Call runs `ubus call <object> <method> [<json args>]` and decodes the reply
// into out (which may be nil).
func (c *Client) Call(ctx context.Context, object, method string, args any, out any) error {
	argv := []string{"call", object, method}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		argv = append(argv, string(b))
	}
	stdout, err := c.run(ctx, argv...)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(stdout)) == 0 {
		return nil
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		return fmt.Errorf("ubus call %s %s: bad reply: %w", object, method, err)
	}
	return nil
}

// List returns the object names matching a pattern (`hostapd.*`).
func (c *Client) List(ctx context.Context, pattern string) ([]string, error) {
	stdout, err := c.run(ctx, "list", pattern)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(string(stdout), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	if c.Run == nil {
		return nil, ErrUnavailable
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// -t: ubus' own timeout, in seconds, a little under ours.
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	full := append([]string{"-t", fmt.Sprint(secs)}, args...)
	stdout, stderr, code, err := c.Run(ctx, c.Bin, full...)
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, ErrUnavailable
		}
		if code < 0 {
			return nil, fmt.Errorf("ubus %s: %w", strings.Join(args, " "), err)
		}
	}
	switch code {
	case 0:
		return stdout, nil
	case 4: // UBUS_STATUS_NOT_FOUND
		return nil, fmt.Errorf("%w: %s", ErrNotFound, strings.Join(args[:min(len(args), 3)], " "))
	default:
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", code)
		}
		return nil, fmt.Errorf("ubus %s: %s", strings.Join(args[:min(len(args), 3)], " "), msg)
	}
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
			err = nil
		} else {
			code = -1
		}
	}
	return stdout.Bytes(), stderr.Bytes(), code, err
}
