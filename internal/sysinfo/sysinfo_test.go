package sysinfo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeUbus struct{ reply map[string]any }

func (f fakeUbus) Call(_ context.Context, object, method string, _ any, out any) error {
	if f.reply == nil {
		return errors.New("no ubus")
	}
	b := out.(*struct {
		Kernel    string `json:"kernel"`
		Hostname  string `json:"hostname"`
		System    string `json:"system"`
		Model     string `json:"model"`
		BoardName string `json:"board_name"`
		Release   struct {
			Distribution string `json:"distribution"`
			Version      string `json:"version"`
			Revision     string `json:"revision"`
			Target       string `json:"target"`
			Description  string `json:"description"`
		} `json:"release"`
	})
	b.Hostname, b.Model, b.BoardName = "ap-1", "Example AP", "example,ap"
	b.Release.Distribution, b.Release.Version, b.Release.Target = "OpenWrt", "24.10.2", "ramips/mt7621"
	return nil
}

func TestBoardFromUbusThenFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.MkdirAll(filepath.Join(root, "proc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/openwrt_release"), []byte("DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='24.10.2'\nDISTRIB_REVISION=\"r28739\"\nDISTRIB_ARCH='mipsel_24kc'\n"), 0o644)
	os.WriteFile(filepath.Join(root, "proc/cpuinfo"), []byte("system type\t\t: MediaTek MT7621 ver:1 eco:3\ncpu model\t\t: MIPS 1004Kc V2.15\n"), 0o644)
	os.WriteFile(filepath.Join(root, "proc/uptime"), []byte("12.50 20.00\n"), 0o644)

	info := &Info{Root: root, Ubus: fakeUbus{reply: map[string]any{}}}
	b := info.Board(context.Background())
	if b.Hostname != "ap-1" || b.Model != "Example AP" || b.Release != "24.10.2" || b.Revision != "r28739" || b.System != "MediaTek MT7621 ver:1 eco:3" {
		t.Fatalf("%+v", b)
	}
	if info.Arch() != "mipsel_24kc" || !info.IsOpenWrt() {
		t.Fatal("arch / openwrt")
	}
	if up, err := info.Uptime(); err != nil || up != 12.5 {
		t.Fatal(up, err)
	}

	files := &Info{Root: root}
	b = files.Board(context.Background())
	if b.Distribution != "OpenWrt" || b.Release != "24.10.2" || b.Model != "" {
		t.Fatalf("files only: %+v", b)
	}
}

func TestCPUSystem(t *testing.T) {
	cases := map[string]string{
		"processor\t: 0\nmodel name\t: ARMv8 Processor rev 4\nHardware\t: Qualcomm IPQ8074\n": "Qualcomm IPQ8074",
		"processor : 0\nmodel name : Intel(R) Celeron(R) N5105\n":                             "Intel(R) Celeron(R) N5105",
		"": "",
	}
	for in, want := range cases {
		if got := cpuSystem(in); got != want {
			t.Errorf("cpuSystem(%q) = %q, want %q", in, got, want)
		}
	}
}
