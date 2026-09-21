package uci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# managed by hand
config agent 'main'
	option enabled '1'
	option controller "https://metrics.example.com"
	# the token goes away after joining
	option join_token 'mlap_abc'
	list extra 'a'
	list extra b

config other 'x'
	option controller 'nope'
`

func TestParseAndGet(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := f.Get("main", "controller"); !ok || v != "https://metrics.example.com" {
		t.Fatalf("controller = %q %v", v, ok)
	}
	if v, _ := f.Get("x", "controller"); v != "nope" {
		t.Fatalf("x.controller = %q", v)
	}
	if got := f.GetList("main", "extra"); strings.Join(got, ",") != "a,b" {
		t.Fatalf("list = %v", got)
	}
	if _, ok := f.Get("main", "missing"); ok {
		t.Fatal("missing option reported present")
	}
	if !f.HasSection("main") || f.HasSection("nope") {
		t.Fatal("HasSection")
	}
}

func TestSetKeepsEverythingElse(t *testing.T) {
	f, _ := Parse([]byte(sample))
	if err := f.Set("main", "agent_id", "4b9d"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("main", "controller", "https://other.example.com"); err != nil {
		t.Fatal(err)
	}
	f.Delete("main", "join_token")
	got := string(f.Bytes())
	want := `# managed by hand
config agent 'main'
	option enabled '1'
	option controller 'https://other.example.com'
	# the token goes away after joining
	list extra 'a'
	list extra b
	option agent_id '4b9d'

config other 'x'
	option controller 'nope'
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// Round trip.
	f2, _ := Parse([]byte(got))
	if v, _ := f2.Get("main", "agent_id"); v != "4b9d" {
		t.Fatalf("agent_id after round trip = %q", v)
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, v := range []string{"", "plain", "it's", `a"b`, `back\slash`, "sp ace", "#hash"} {
		f, _ := Parse(nil)
		f.EnsureSection("agent", "main")
		if err := f.Set("main", "v", v); err != nil {
			t.Fatal(err)
		}
		f2, _ := Parse(f.Bytes())
		if got, _ := f2.Get("main", "v"); got != v {
			t.Fatalf("round trip %q -> %q (file %q)", v, got, f.Bytes())
		}
	}
}

func TestEnsureSectionAndSetOnEmpty(t *testing.T) {
	f, _ := Parse([]byte("config other 'x'\n\toption a '1'\n"))
	f.EnsureSection("agent", "main")
	if err := f.Set("main", "enabled", "1"); err != nil {
		t.Fatal(err)
	}
	want := "config other 'x'\n\toption a '1'\n\nconfig agent 'main'\n\toption enabled '1'\n"
	if got := string(f.Bytes()); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if err := f.Set("nope", "a", "b"); err == nil {
		t.Fatal("Set on a missing section should fail")
	}
	if err := f.Set("main", "bad-name", "b"); err == nil {
		t.Fatal("invalid option name accepted")
	}
}

func TestWriteFileAtomicKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perch-apd")
	if err := os.WriteFile(path, []byte("config agent 'main'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Set("main", "x", "y")
	if err := f.WriteFile(path, 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "config agent 'main'\n\toption x 'y'\n" {
		t.Fatalf("content %q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	missing, err := Load(filepath.Join(dir, "nope"))
	if err != nil || missing.HasSection("main") {
		t.Fatalf("Load(missing) = %v, %v", missing, err)
	}
}

func TestUnterminatedQuoteKeptAsIs(t *testing.T) {
	f, _ := Parse([]byte("config agent 'main'\n\toption bad 'oops\n\toption ok 'fine'\n"))
	if _, ok := f.Get("main", "bad"); ok {
		t.Fatal("broken line parsed as an option")
	}
	if v, _ := f.Get("main", "ok"); v != "fine" {
		t.Fatalf("ok = %q", v)
	}
	if !strings.Contains(string(f.Bytes()), "option bad 'oops") {
		t.Fatal("broken line not preserved")
	}
}
