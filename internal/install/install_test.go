package install

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capthndsme/perch-apd/internal/agent"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
)

type harness struct {
	env    *Env
	root   string
	out    *bytes.Buffer
	runs   []string
	joins  []agent.JoinRequest
	status int
	srv    *httptest.Server
}

func newHarness(t *testing.T, stdin string) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{root: root, out: &bytes.Buffer{}}
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, OpenWrtRelease), []byte("DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='25.12.4'\nDISTRIB_ARCH='mipsel_24kc'\n"), 0o644)
	self := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(self, []byte("#!binary"), 0o755)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ap-agent/join" {
			http.NotFound(w, r)
			return
		}
		var req agent.JoinRequest
		json.NewDecoder(r.Body).Decode(&req)
		h.joins = append(h.joins, req)
		if h.status != 0 {
			w.WriteHeader(h.status)
			io.WriteString(w, `{"error":"invalid_join_token","message":"This join token was revoked."}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"data":{"agentId":"id1","agentSecret":"sec1","apId":4,"apName":"ap-garage","outcome":"linked"}}`)
	}))
	t.Cleanup(h.srv.Close)
	h.env = &Env{
		Root:   root,
		Stdin:  strings.NewReader(stdin),
		Stdout: h.out,
		Self:   self,
		IsRoot: true,
		Run: func(name string, args ...string) error {
			h.runs = append(h.runs, strings.TrimPrefix(name, root)+" "+strings.Join(args, " "))
			return nil
		},
		HTTP: h.srv.Client(),
		Info: &sysinfo.Info{Root: root},
	}
	return h
}

func (h *harness) read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallInteractive(t *testing.T) {
	h := newHarness(t, "")
	h.env.Stdin = strings.NewReader(h.srv.URL + "\nmlap_token\n")
	if err := h.env.Install(context.Background(), Options{}); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if h.read(t, OptBin) != "#!binary" {
		t.Fatal("binary not copied")
	}
	st, _ := os.Stat(filepath.Join(h.root, OptBin))
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode %v", st.Mode())
	}
	if h.read(t, InitPath) != string(InitScript) {
		t.Fatal("init script differs")
	}
	if !strings.Contains(h.read(t, KeepFile), "/opt/perch-apd/") {
		t.Fatal("keep.d entry missing")
	}
	cfg, err := config.Load(filepath.Join(h.root, ConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Controller != h.srv.URL || cfg.AgentID != "id1" || cfg.AgentSecret != "sec1" || cfg.JoinToken != "" {
		t.Fatalf("config %+v", cfg)
	}
	if len(h.joins) != 1 || h.joins[0].Token != "mlap_token" || h.joins[0].Release != "25.12.4" {
		t.Fatalf("joins %+v", h.joins)
	}
	if strings.Join(h.runs, ";") != InitPath+" enable;"+InitPath+" stop;"+InitPath+" start" {
		t.Fatalf("runs %v", h.runs)
	}
	out := h.out.String()
	if !strings.Contains(out, `Joined as access point #4 "ap-garage": linked`) {
		t.Fatalf("output:\n%s", out)
	}
	st, _ = os.Stat(filepath.Join(h.root, ConfigPath))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", st.Mode())
	}
}

func TestInstallNonInteractiveRequiresFlags(t *testing.T) {
	h := newHarness(t, "")
	if err := h.env.Install(context.Background(), Options{Yes: true}); err == nil || !strings.Contains(err.Error(), "--controller") {
		t.Fatalf("err = %v", err)
	}
	if err := h.env.Install(context.Background(), Options{Yes: true, Controller: h.srv.URL}); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallRefusedJoinIsAnError(t *testing.T) {
	h := newHarness(t, "")
	h.status = http.StatusUnauthorized
	err := h.env.Install(context.Background(), Options{Yes: true, Controller: h.srv.URL, Token: "bad"})
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("err = %v", err)
	}
	if len(h.runs) != 0 {
		t.Fatal("service started after a refused join")
	}
}

func TestInstallUnreachableControllerKeepsTokenAndStarts(t *testing.T) {
	h := newHarness(t, "")
	h.srv.Close() // nothing listens any more
	err := h.env.Install(context.Background(), Options{Yes: true, Controller: h.srv.URL, Token: "later"})
	if err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	cfg, _ := config.Load(filepath.Join(h.root, ConfigPath))
	if cfg.JoinToken != "later" || cfg.HasCredentials() {
		t.Fatalf("config %+v", cfg)
	}
	if len(h.runs) != 3 {
		t.Fatalf("runs %v", h.runs)
	}
}

func TestInstallOverPackageDoesNotCopy(t *testing.T) {
	h := newHarness(t, "")
	os.MkdirAll(filepath.Join(h.root, "usr/bin"), 0o755)
	os.WriteFile(filepath.Join(h.root, PkgBin), []byte("pkg"), 0o755)
	os.MkdirAll(filepath.Join(h.root, "etc/init.d"), 0o755)
	os.WriteFile(filepath.Join(h.root, InitPath), []byte("package init"), 0o755)
	if err := h.env.Install(context.Background(), Options{Yes: true, Controller: h.srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.root, OptBin)); !os.IsNotExist(err) {
		t.Fatal("copied a second binary next to the package")
	}
	if h.read(t, InitPath) != "package init" {
		t.Fatal("overwrote the package's init script")
	}
}

func TestReinstallKeepsCredentialsUnlessRejoin(t *testing.T) {
	h := newHarness(t, "")
	if err := h.env.Install(context.Background(), Options{Yes: true, Controller: h.srv.URL, Token: "t1"}); err != nil {
		t.Fatal(err)
	}
	// Second run, same controller, no token, Enter at the prompt: keeps credentials.
	h.env.Stdin = strings.NewReader("\n\n")
	h.env.in = nil
	if err := h.env.Install(context.Background(), Options{}); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if len(h.joins) != 1 {
		t.Fatalf("joined again: %d", len(h.joins))
	}
	cfg, _ := config.Load(filepath.Join(h.root, ConfigPath))
	if cfg.AgentID != "id1" {
		t.Fatalf("credentials lost: %+v", cfg)
	}
	// A different controller drops the old credentials and needs a token.
	h.env.Stdin = strings.NewReader("")
	h.env.in = nil
	err := h.env.Install(context.Background(), Options{Yes: true, Controller: "https://other.example.com"})
	if err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("err = %v", err)
	}
}

func TestJoinCommand(t *testing.T) {
	h := newHarness(t, "")
	os.MkdirAll(filepath.Join(h.root, "etc/init.d"), 0o755)
	os.WriteFile(filepath.Join(h.root, InitPath), InitScript, 0o755)
	if err := h.env.Join(context.Background(), Options{Controller: "  " + h.srv.URL + "/ ", Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	if len(h.joins) != 1 || h.joins[0].Token != "t2" {
		t.Fatalf("joins %+v", h.joins)
	}
	if strings.Join(h.runs, ";") != InitPath+" enable;"+InitPath+" stop;"+InitPath+" start" {
		t.Fatalf("runs %v", h.runs)
	}
}

func TestUninstall(t *testing.T) {
	h := newHarness(t, "")
	if err := h.env.Install(context.Background(), Options{Yes: true, Controller: h.srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	h.runs = nil
	if err := h.env.Uninstall(UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{OptBin, InitPath, KeepFile} {
		if _, err := os.Stat(filepath.Join(h.root, p)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind", p)
		}
	}
	if _, err := os.Stat(filepath.Join(h.root, ConfigPath)); err != nil {
		t.Fatal("config removed without --purge")
	}
	if strings.Join(h.runs, ";") != InitPath+" stop;"+InitPath+" disable" {
		t.Fatalf("runs %v", h.runs)
	}
	if err := h.env.Uninstall(UninstallOptions{Purge: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.root, ConfigPath)); !os.IsNotExist(err) {
		t.Fatal("config kept with --purge")
	}
}

func TestRefusals(t *testing.T) {
	h := newHarness(t, "")
	h.env.IsRoot = false
	if err := h.env.Install(context.Background(), Options{}); err == nil {
		t.Fatal("installed as non-root")
	}
	h.env.IsRoot = true
	os.Remove(filepath.Join(h.root, OpenWrtRelease))
	if err := h.env.Install(context.Background(), Options{Yes: true}); err == nil || !strings.Contains(err.Error(), "OpenWrt") {
		t.Fatalf("err = %v", err)
	}
}

// The package's files must be the ones the binary installs.
func TestOpenWrtPackageFilesInSync(t *testing.T) {
	pkgInit, err := os.ReadFile("../../openwrt/perch-apd/files/perch-apd.init")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pkgInit, InitScript) {
		t.Fatal("openwrt/perch-apd/files/perch-apd.init differs from internal/install/perch-apd.init")
	}
	pkgConf, err := os.ReadFile("../../openwrt/perch-apd/files/perch-apd.config")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pkgConf, config.DefaultFile) {
		t.Fatal("openwrt/perch-apd/files/perch-apd.config differs from internal/config/default.conf")
	}
}
