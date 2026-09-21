package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndMissingFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.Controller != "" || c.LogLevel != "info" || c.HasCredentials() {
		t.Fatalf("defaults = %+v", c)
	}

	path := filepath.Join(t.TempDir(), "perch-apd")
	if err := os.WriteFile(path, DefaultFile, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.Controller != "" || c.JoinToken != "" || c.TLSInsecure {
		t.Fatalf("default file = %+v", c)
	}
}

func TestSaveCredentialsKeepsCommentsAndClearsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(path, DefaultFile, 0o644)
	if err := Update(path, map[string]*string{
		"controller": ptr("metrics.example.com/"),
		"join_token": ptr("mlap_x"),
	}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Controller != "https://metrics.example.com" || c.JoinToken != "mlap_x" {
		t.Fatalf("after update: %+v", c)
	}
	if err := c.SaveCredentials("abc", "s3cret"); err != nil {
		t.Fatal(err)
	}
	c2, _ := Load(path)
	if c2.AgentID != "abc" || c2.AgentSecret != "s3cret" || c2.JoinToken != "" {
		t.Fatalf("after save: %+v", c2)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# Join token from the dashboard") {
		t.Fatal("comments lost")
	}
	if strings.Count(string(data), "option join_token") != 1 {
		t.Fatalf("join_token line duplicated or removed:\n%s", data)
	}
	if err := c2.ClearCredentials(); err != nil {
		t.Fatal(err)
	}
	c3, _ := Load(path)
	if c3.HasCredentials() || c3.Controller == "" {
		t.Fatalf("after clear: %+v", c3)
	}
}

func TestUpdateCreatesFileFromDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "perch-apd")
	if err := Update(path, map[string]*string{"controller": ptr("http://192.168.1.10:8080")}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# perch-apd: Perch AP Daemon.") {
		t.Fatalf("not created from defaults:\n%s", data)
	}
	c, _ := Load(path)
	if c.Controller != "http://192.168.1.10:8080" {
		t.Fatalf("controller %q", c.Controller)
	}
}

func TestNormalizeControllerURL(t *testing.T) {
	cases := map[string]string{
		"metrics.example.com":             "https://metrics.example.com",
		"https://metrics.example.com/":    "https://metrics.example.com",
		"http://192.168.1.10:8080":        "http://192.168.1.10:8080",
		" https://example.com/metrics// ": "https://example.com/metrics",
		"https://example.com/?x=1#frag":   "https://example.com",
	}
	for in, want := range cases {
		got, err := NormalizeControllerURL(in)
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://", "https://user:pw@example.com"} {
		if _, err := NormalizeControllerURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBadControllerIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c")
	os.WriteFile(path, []byte("config agent 'main'\n\toption controller 'ftp://x'\n"), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("bad controller accepted")
	}
}

func ptr(s string) *string { return &s }
