package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/capthndsme/perch-agentkit/link"
	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
)

// An apply drops the session and dials a new one at once; every session
// has its own number and challenge, agent.configure reaches the plane, and
// the plane hears about each session's end.
func TestReconnectDialsAFreshSessionAtOnce(t *testing.T) {
	type who struct {
		Gen       uint64 `json:"gen"`
		Challenge string `json:"challenge"`
	}
	fc := &fakeController{t: t}
	whos := make(chan who, 4)
	closes := make(chan websocket.CloseError, 4)
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"agent.configure","params":{"metricsIntervalSeconds":0,"wifiConfig":{"mode":"managed"}}}`))
		m := rpcCall(t, ctx, c, `{"jsonrpc":"2.0","id":1,"method":"whoami"}`)
		var w who
		json.Unmarshal(m.Result, &w)
		whos <- w
		for {
			_, _, err := c.Read(ctx)
			if err == nil {
				continue // the notification sent meanwhile
			}
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				closes <- ce
			}
			return
		}
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(path, []byte("config agent 'main'\n\toption controller '"+srv.URL+"'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n"), 0o600)
	cfg, _ := config.Load(path)
	var mu sync.Mutex
	var configures []string
	ends := 0
	var a *Agent
	disp := rpc.NewDispatcher()
	disp.Register("whoami", func(ctx context.Context, _ json.RawMessage) (any, error) {
		gen, ch := a.SessionRef(ctx)
		return who{gen, ch}, nil
	})
	sleeps := make(chan time.Duration, 8)
	a, err := New(Options{
		Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Dispatcher: disp,
		Info: &sysinfo.Info{Root: t.TempDir()},
		Sleep: func(ctx context.Context, d time.Duration) error {
			sleeps <- d
			return nil
		},
		OnConfigure: func(p json.RawMessage) {
			mu.Lock()
			configures = append(configures, string(p))
			mu.Unlock()
		},
		OnSessionEnd: func() {
			mu.Lock()
			ends++
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	first := <-whos
	for !a.Connected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !a.Send("wifi.config.changed", map[string]any{"changed": []string{"wireless"}}) {
		t.Fatal("send on an open session")
	}
	a.Reconnect("reconnecting after apply a4-000000000001")
	select {
	case ce := <-closes:
		if ce.Code != websocket.StatusNormalClosure || ce.Reason != "reconnecting after apply a4-000000000001" {
			t.Fatalf("close %v %q", ce.Code, ce.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session was not closed")
	}
	if d := <-sleeps; d != 0 {
		t.Fatalf("redial after %v, want at once", d)
	}
	second := <-whos
	if first.Gen == 0 || second.Gen <= first.Gen || len(first.Challenge) != 32 || first.Challenge == second.Challenge {
		t.Fatalf("sessions %+v %+v", first, second)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(configures) < 2 || configures[0] != `{"metricsIntervalSeconds":0,"wifiConfig":{"mode":"managed"}}` || ends < 1 {
		t.Fatalf("configures %v, ends %d", configures, ends)
	}
}

func TestRedialDelay(t *testing.T) {
	fast := true
	a := &Agent{redialFast: func() bool { return fast }}
	if d := a.redialDelay(errors.New("connection refused"), 16*time.Second); d != FastRedial {
		t.Fatalf("while the plane waits: %v", d)
	}
	if d := a.redialDelay(errors.New("connection refused"), time.Second); d != time.Second {
		t.Fatalf("a shorter backoff: %v", d)
	}
	// Refusals keep their delay: credentials, a missing endpoint, Retry-After.
	for _, err := range []error{&link.StatusError{Status: http.StatusUnauthorized}, &link.StatusError{Status: http.StatusNotFound},
		&link.StatusError{Status: http.StatusServiceUnavailable, RetryAfter: 7 * time.Second}} {
		if d := a.redialDelay(err, slowRetry); d != slowRetry {
			t.Fatalf("%v: %v", err, d)
		}
	}
	fast = false
	if d := a.redialDelay(errors.New("x"), 16*time.Second); d != 16*time.Second {
		t.Fatalf("no plane waiting: %v", d)
	}
	a.redialNow = true
	if d := a.redialDelay(errors.New("x"), 16*time.Second); d != 0 || a.redialNow {
		t.Fatalf("after Reconnect: %v %v", d, a.redialNow)
	}
	// Reconnect without a session does nothing (the loop is dialing).
	a.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a.Reconnect("x")
	if a.redialNow {
		t.Fatal("redial flagged without a session")
	}
	if a.Send("x", nil) {
		t.Fatal("sent without a session")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.beginSession()
	if gen, ch := a.SessionRef(ctx); gen != 1 || ch == "" {
		t.Fatalf("session %d %q", gen, ch)
	}
	cancel()
	if gen, ch := a.SessionRef(ctx); gen != 0 || ch != "" {
		t.Fatalf("an ended session's request: %d %q", gen, ch)
	}
}

// OnSessionOpen gets a notifier on every open session (the updater re-sends
// its unacknowledged results there).
func TestSessionOpenHookNotifies(t *testing.T) {
	fc := &fakeController{t: t}
	got := make(chan string, 4)
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m rpc.Message
			if json.Unmarshal(b, &m) == nil && m.Method == "agent.update.result" {
				got <- string(m.Params)
			}
		}
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(path, []byte("config agent 'main'\n\toption controller '"+srv.URL+"'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n"), 0o600)
	cfg, _ := config.Load(path)
	a, err := New(Options{
		Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Info: &sysinfo.Info{Root: t.TempDir()},
		OnSessionOpen: func(notify func(string, any) error) {
			if err := notify("agent.update.result", map[string]string{"updateId": "u-0000000000000001"}); err != nil {
				t.Errorf("notify: %v", err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case p := <-got:
		if p != `{"updateId":"u-0000000000000001"}` {
			t.Fatalf("params %s", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification on the open session")
	}
}
