// Package config is the daemon's view of /etc/config/perch-apd.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/capthndsme/perch-apd/internal/uci"
)

// DefaultPath is where OpenWrt keeps the agent's UCI configuration.
const DefaultPath = "/etc/config/perch-apd"

// Section is the one UCI section the agent reads: `config agent 'main'`.
const (
	SectionType = "agent"
	SectionName = "main"
)

// DefaultFile is the commented default configuration, shipped by the
// OpenWrt package and written by `perch-apd install`.
//
//go:embed default.conf
var DefaultFile []byte

// Config holds the settings the daemon runs with.
type Config struct {
	Path        string
	Enabled     bool
	Controller  string // normalized base URL, "" = none
	JoinToken   string
	AgentID     string
	AgentSecret string
	TLSInsecure bool
	CAFile      string
	LogLevel    string
}

// HasCredentials reports whether the agent has joined a controller.
func (c *Config) HasCredentials() bool { return c.AgentID != "" && c.AgentSecret != "" }

// Load reads the configuration. A missing file yields the defaults.
func Load(path string) (*Config, error) {
	f, err := uci.Load(path)
	if err != nil {
		return nil, err
	}
	get := func(opt, def string) string {
		if v, ok := f.Get(SectionName, opt); ok {
			return strings.TrimSpace(v)
		}
		return def
	}
	c := &Config{
		Path:        path,
		Enabled:     parseBool(get("enabled", "1"), true),
		JoinToken:   get("join_token", ""),
		AgentID:     get("agent_id", ""),
		AgentSecret: get("agent_secret", ""),
		TLSInsecure: parseBool(get("tls_insecure", "0"), false),
		CAFile:      get("ca_file", ""),
		LogLevel:    get("log_level", "info"),
	}
	if raw := get("controller", ""); raw != "" {
		u, err := NormalizeControllerURL(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: controller: %w", path, err)
		}
		c.Controller = u
	}
	return c, nil
}

// SaveCredentials stores what a successful join returned and forgets the
// join token, which is not needed any more.
func (c *Config) SaveCredentials(agentID, agentSecret string) error {
	err := Update(c.Path, map[string]*string{
		"agent_id":     &agentID,
		"agent_secret": &agentSecret,
		"join_token":   nil,
	})
	if err != nil {
		return err
	}
	c.AgentID, c.AgentSecret, c.JoinToken = agentID, agentSecret, ""
	return nil
}

// ClearCredentials drops agent_id/agent_secret (after the controller revoked
// them), keeping everything else.
func (c *Config) ClearCredentials() error {
	err := Update(c.Path, map[string]*string{"agent_id": nil, "agent_secret": nil})
	if err != nil {
		return err
	}
	c.AgentID, c.AgentSecret = "", ""
	return nil
}

// Update sets (non-nil) or deletes (nil) options of the main section,
// creating the file from the defaults when it does not exist yet. The file
// is written atomically with mode 0600: it may hold the agent secret.
func Update(path string, values map[string]*string) error {
	f, err := uci.Load(path)
	if err != nil {
		return err
	}
	if !f.HasSection(SectionName) {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			f, _ = uci.Parse(DefaultFile)
		}
		f.EnsureSection(SectionType, SectionName)
	}
	for _, key := range sortedKeys(values) {
		if v := values[key]; v != nil {
			if err := f.Set(SectionName, key, *v); err != nil {
				return err
			}
		} else {
			// Keep the documented empty option rather than removing the line.
			if _, ok := f.Get(SectionName, key); ok {
				if err := f.Set(SectionName, key, ""); err != nil {
					return err
				}
			}
		}
	}
	return f.WriteFile(path, 0o600)
}

// NormalizeControllerURL accepts what an admin types ("perch.example.com",
// "https://perch.example.com/", "http://192.168.1.10:8080") and returns a
// base URL without a trailing slash. A missing scheme means https.
func NormalizeControllerURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty URL")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported scheme %q (use http or https)", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("missing host")
	}
	if u.User != nil {
		return "", errors.New("credentials in the URL are not supported")
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "yes", "on", "true", "enabled":
		return true
	case "0", "no", "off", "false", "disabled":
		return false
	}
	return def
}

func sortedKeys(m map[string]*string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
