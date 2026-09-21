package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/capthndsme/perch-agentkit/link"
	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
)

type fakeController struct {
	t          *testing.T
	mu         sync.Mutex
	joins      []JoinRequest
	joinStatus int
	joinBody   string
	authHeader []string
	wsStatus   int // non-zero: refuse the upgrade with this status
	onSession  func(ctx context.Context, c *websocket.Conn)
	subproto   []string
	extensions []string // Sec-WebSocket-Extensions of each upgrade
	compress   bool     // accept permessage-deflate like the controller does
}

func (f *fakeController) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ap-agent/join", func(w http.ResponseWriter, r *http.Request) {
		var req JoinRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.joins = append(f.joins, req)
		status, body := f.joinStatus, f.joinBody
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusCreated
			body = `{"data":{"agentId":"4b9d0c1e2f3a4b5c6d7e8f9012345678","agentSecret":"secret-1","apId":4,"apName":"ap-garage","outcome":"linked"}}`
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	mux.HandleFunc("/api/v1/ap-agent/ws", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authHeader = append(f.authHeader, r.Header.Get("Authorization"))
		f.subproto = append(f.subproto, r.Header.Get("Sec-WebSocket-Protocol"))
		f.extensions = append(f.extensions, r.Header.Get("Sec-WebSocket-Extensions"))
		status := f.wsStatus
		onSession := f.onSession
		mode := websocket.CompressionDisabled
		if f.compress {
			mode = websocket.CompressionNoContextTakeover
		}
		f.mu.Unlock()
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"invalid_agent_credentials"}`)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}, CompressionMode: mode})
		if err != nil {
			f.t.Errorf("accept: %v", err)
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(4 << 20) // the controller's maxPayload for AP sessions
		if onSession != nil {
			onSession(r.Context(), c)
		}
	})
	return mux
}

func newTestAgent(t *testing.T, srvURL string, cfgText string, sleeps chan<- time.Duration) (*Agent, *config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(path, []byte(strings.ReplaceAll(cfgText, "URL", srvURL)), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	disp := rpc.NewDispatcher()
	disp.Register("ping", func(context.Context, json.RawMessage) (any, error) {
		return map[string]bool{"pong": true}, nil
	})
	a, err := New(Options{
		Config:     cfg,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: disp,
		Info:       &sysinfo.Info{Root: t.TempDir()},
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case sleeps <- d:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, cfg
}

func rpcCall(t *testing.T, ctx context.Context, c *websocket.Conn, frame string) rpc.Message {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m rpc.Message
	json.Unmarshal(data, &m)
	return m
}

func TestJoinThenServeRequestsAndReconnect(t *testing.T) {
	fc := &fakeController{t: t}
	sessions := make(chan string, 4)
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		m := rpcCall(t, ctx, c, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		if m.Error != nil || string(m.Result) != `{"pong":true}` {
			t.Errorf("ping result %s %+v", m.Result, m.Error)
		}
		m = rpcCall(t, ctx, c, `{"jsonrpc":"2.0","id":"x","method":"nope"}`)
		if m.Error == nil || m.Error.Code != rpc.CodeMethodNotFound || string(m.ID) != `"x"` {
			t.Errorf("unknown method %+v", m)
		}
		sessions <- "served"
		c.Close(websocket.StatusNormalClosure, "bye")
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()

	sleeps := make(chan time.Duration, 16)
	a, cfg := newTestAgent(t, srv.URL, "config agent 'main'\n\toption controller 'URL'\n\toption join_token 'mlap_test'\n", sleeps)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	<-sessions
	if d := <-sleeps; d < 500*time.Millisecond || d > 2*time.Second {
		t.Fatalf("first reconnect delay %v", d) // backoff starts at ~1 s
	}
	<-sessions // reconnected with the stored credentials, no second join
	<-sleeps
	cancel()
	<-done

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.joins) != 1 || fc.joins[0].Token != "mlap_test" || fc.joins[0].AgentVersion == "" || fc.joins[0].MACs == nil {
		t.Fatalf("joins %+v", fc.joins)
	}
	if fc.authHeader[0] != "Bearer 4b9d0c1e2f3a4b5c6d7e8f9012345678.secret-1" || fc.subproto[0] != Subprotocol {
		t.Fatalf("handshake headers %v %v", fc.authHeader, fc.subproto)
	}
	stored, _ := config.Load(cfg.Path)
	if stored.AgentID != "4b9d0c1e2f3a4b5c6d7e8f9012345678" || stored.AgentSecret != "secret-1" || stored.JoinToken != "" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestCloseCodesAndStatusesPickTheRetryDelay(t *testing.T) {
	cases := []struct {
		name  string
		setup func(fc *fakeController)
		cfg   string
		want  time.Duration
	}{
		{"ws 401 without token", func(fc *fakeController) { fc.wsStatus = http.StatusUnauthorized },
			"config agent 'main'\n\toption controller 'URL'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n", slowRetry},
		{"ws 404", func(fc *fakeController) { fc.wsStatus = http.StatusNotFound },
			"config agent 'main'\n\toption controller 'URL'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n", slowRetry},
		{"close 4002", func(fc *fakeController) {
			fc.onSession = func(ctx context.Context, c *websocket.Conn) { c.Close(link.CloseReplaced, "replaced") }
		}, "config agent 'main'\n\toption controller 'URL'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n", replacedRetry},
		{"close 4001 without token", func(fc *fakeController) {
			fc.onSession = func(ctx context.Context, c *websocket.Conn) { c.Close(link.CloseRevoked, "revoked") }
		}, "config agent 'main'\n\toption controller 'URL'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n", slowRetry},
		{"join 401", func(fc *fakeController) {
			fc.joinStatus, fc.joinBody = http.StatusUnauthorized, `{"error":"invalid_join_token","message":"expired"}`
		}, "config agent 'main'\n\toption controller 'URL'\n\toption join_token 't'\n", slowRetry},
		{"join 429", func(fc *fakeController) {
			fc.joinStatus, fc.joinBody = http.StatusTooManyRequests, `{"error":"rate_limited"}`
		},
			"config agent 'main'\n\toption controller 'URL'\n\toption join_token 't'\n", 7 * time.Second},
		{"no token, no credentials", func(fc *fakeController) {},
			"config agent 'main'\n\toption controller 'URL'\n", slowRetry},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc := &fakeController{t: t}
			c.setup(fc)
			srv := httptest.NewServer(fc.handler())
			defer srv.Close()
			sleeps := make(chan time.Duration, 4)
			a, _ := newTestAgent(t, srv.URL, c.cfg, sleeps)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { a.Run(ctx); close(done) }()
			select {
			case d := <-sleeps:
				if d != c.want {
					t.Fatalf("delay %v, want %v", d, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no retry")
			}
			cancel()
			<-done
		})
	}
}

func TestRevokedWithTokenRejoins(t *testing.T) {
	fc := &fakeController{t: t}
	var mu sync.Mutex
	n := 0
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		mu.Lock()
		n++
		mu.Unlock()
		c.Close(link.CloseRevoked, "revoked")
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	sleeps := make(chan time.Duration, 4)
	a, _ := newTestAgent(t, srv.URL,
		"config agent 'main'\n\toption controller 'URL'\n\toption agent_id 'old'\n\toption agent_secret 'old'\n\toption join_token 'fresh'\n", sleeps)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	<-sleeps // after the second (post-join) session is revoked too: no token left
	cancel()
	<-done
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.joins) != 1 || fc.joins[0].Token != "fresh" {
		t.Fatalf("joins %+v", fc.joins)
	}
	if fc.authHeader[0] != "Bearer old.old" || fc.authHeader[1] != "Bearer 4b9d0c1e2f3a4b5c6d7e8f9012345678.secret-1" {
		t.Fatalf("auth %v", fc.authHeader)
	}
}

func TestShutdownSendsGoingAway(t *testing.T) {
	fc := &fakeController{t: t}
	got := make(chan websocket.StatusCode, 1)
	connected := make(chan struct{})
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		close(connected)
		_, _, err := c.Read(ctx)
		got <- websocket.CloseStatus(err)
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	sleeps := make(chan time.Duration, 4)
	a, _ := newTestAgent(t, srv.URL, "config agent 'main'\n\toption controller 'URL'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n", sleeps)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	<-connected
	if !a.Connected() {
		time.Sleep(50 * time.Millisecond)
	}
	a.Notify("locate.ended", map[string]string{"reason": "timeout"})
	cancel()
	select {
	case code := <-got:
		// The notification arrives first; the read loop in the fake stops there.
		if code != -1 && code != websocket.StatusGoingAway {
			t.Fatalf("close code %v", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the agent leave")
	}
	<-done
}

type fakeMetrics struct {
	mu    sync.Mutex
	calls [][]string
	text  []byte // what Gather returns; nil = one load average
}

func (f *fakeMetrics) Gather(_ context.Context, names []string) []byte {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), names...))
	f.mu.Unlock()
	if f.text != nil {
		return f.text
	}
	return []byte("# TYPE node_load1 gauge\nnode_load1 0.04\n")
}

func (f *fakeMetrics) last() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

type pushed struct {
	at     time.Time
	params struct {
		Format      string `json:"format"`
		Text        string `json:"text"`
		CollectedAt string `json:"collectedAt"`
		Seq         int    `json:"seq"`
	}
}

// readPushes forwards every metrics.push the fake controller receives.
func readPushes(ctx context.Context, t *testing.T, c *websocket.Conn, out chan<- pushed) {
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m rpc.Message
		if json.Unmarshal(data, &m) != nil || m.Method != "metrics.push" || m.ID != nil {
			continue
		}
		var p pushed
		p.at = time.Now()
		if err := json.Unmarshal(m.Params, &p.params); err != nil {
			t.Errorf("push params: %v", err)
			continue
		}
		out <- p
	}
}

func newPushAgent(t *testing.T, srvURL string, mets *fakeMetrics, wait time.Duration) *Agent {
	t.Helper()
	path := filepath.Join(t.TempDir(), "perch-apd")
	os.WriteFile(path, []byte("config agent 'main'\n\toption controller '"+srvURL+"'\n\toption agent_id 'a'\n\toption agent_secret 'b'\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		Config:     cfg,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: rpc.NewDispatcher(),
		Info:       &sysinfo.Info{Root: t.TempDir()},
		Metrics:    mets,
		ConfigWait: wait,
		Sleep: func(ctx context.Context, d time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPushFollowsConfigure(t *testing.T) {
	fc := &fakeController{t: t}
	pushes := make(chan pushed, 16)
	send := make(chan string, 4)
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		go readPushes(ctx, t, c, pushes)
		c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"agent.configure","params":{"metricsIntervalSeconds":1,"collectors":["loadavg"]}}`))
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-send:
				c.Write(ctx, websocket.MessageText, []byte(frame))
			}
		}
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	mets := &fakeMetrics{}
	a := newPushAgent(t, srv.URL, mets, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	first := <-pushes
	if first.params.Seq != 1 || first.params.Format != "prometheus-text" || !strings.Contains(first.params.Text, "node_load1 0.04") || first.params.CollectedAt == "" {
		t.Fatalf("first push %+v", first.params)
	}
	if got := mets.last(); len(got) != 1 || got[0] != "loadavg" {
		t.Fatalf("collectors %v", got)
	}
	select {
	case second := <-pushes:
		gap := second.at.Sub(first.at)
		if second.params.Seq != 2 || gap < 800*time.Millisecond || gap > 2*time.Second {
			t.Fatalf("second push seq %d after %v", second.params.Seq, gap)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no second push")
	}

	// Interval 0 pauses; a later configure resumes on the new schedule.
	send <- `{"jsonrpc":"2.0","method":"agent.configure","params":{"metricsIntervalSeconds":0}}`
	time.Sleep(100 * time.Millisecond)
	for len(pushes) > 0 {
		<-pushes // one may have been in flight
	}
	select {
	case p := <-pushes:
		t.Fatalf("pushed while paused: seq %d", p.params.Seq)
	case <-time.After(1500 * time.Millisecond):
	}
	send <- `{"jsonrpc":"2.0","method":"agent.configure","params":{"metricsIntervalSeconds":1}}`
	select {
	case p := <-pushes:
		if got := mets.last(); strings.Join(got, ",") != strings.Join(DefaultCollectors, ",") {
			t.Fatalf("collectors after a configure without them: %v", got)
		}
		_ = p
	case <-time.After(3 * time.Second):
		t.Fatal("no push after resuming")
	}
}

func TestPushFallsBackWithoutConfigure(t *testing.T) {
	fc := &fakeController{t: t}
	pushes := make(chan pushed, 4)
	fc.onSession = func(ctx context.Context, c *websocket.Conn) {
		readPushes(ctx, t, c, pushes)
	}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	mets := &fakeMetrics{}
	a := newPushAgent(t, srv.URL, mets, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case p := <-pushes:
		if p.params.Seq != 1 || strings.Join(mets.last(), ",") != strings.Join(DefaultCollectors, ",") {
			t.Fatalf("fallback push %+v collectors %v", p.params, mets.last())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no fallback push")
	}
}

func TestParseConfigure(t *testing.T) {
	cases := []struct {
		in       string
		interval time.Duration
		coll     int
	}{
		{`{"metricsIntervalSeconds":5,"collectors":["wifi"]}`, 5 * time.Second, 1},
		{`{"metricsIntervalSeconds":0}`, 0, len(DefaultCollectors)},
		{`{"metricsIntervalSeconds":0.2}`, time.Second, len(DefaultCollectors)},
		{`{"metricsIntervalSeconds":99999}`, time.Hour, len(DefaultCollectors)},
		{`{}`, 15 * time.Second, len(DefaultCollectors)},
	}
	for _, c := range cases {
		cfg, err := ParseConfigure(json.RawMessage(c.in))
		if err != nil || cfg.Interval != c.interval || len(cfg.Collectors) != c.coll {
			t.Errorf("%s -> %+v %v", c.in, cfg, err)
		}
	}
	if _, err := ParseConfigure(json.RawMessage(`{"metricsIntervalSeconds":"x"}`)); err == nil {
		t.Error("bad interval accepted")
	}
}

// countingListener counts the bytes the fake controller reads off the wire.
type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countingConn{c, l.n}, nil
}

type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func TestPushesAreCompressedWhenTheControllerAgrees(t *testing.T) {
	fc := &fakeController{t: t, compress: true}
	pushes := make(chan pushed, 4)
	fc.onSession = func(ctx context.Context, c *websocket.Conn) { readPushes(ctx, t, c, pushes) }
	srv := httptest.NewUnstartedServer(fc.handler())
	var onWire atomic.Int64
	srv.Listener = countingListener{srv.Listener, &onWire}
	srv.Start()
	defer srv.Close()
	text := bytes.Repeat([]byte(`wifi_station_receive_bytes_total{ifname="phy0-ap0",mac="02:00:00:00:00:01"} 7366133107`+"\n"), 400)
	a := newPushAgent(t, srv.URL, &fakeMetrics{text: text}, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	select {
	case p := <-pushes:
		if p.params.Text != string(text) {
			t.Fatal("the push text changed on the way")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no push")
	}
	fc.mu.Lock()
	offered := strings.Join(fc.extensions, "; ")
	fc.mu.Unlock()
	if !strings.Contains(offered, "permessage-deflate") {
		t.Fatalf("offered extensions %q", offered)
	}
	// Upgrade request plus one push: far less than the 36 KB of text.
	if n := onWire.Load(); n > int64(len(text))/4 {
		t.Fatalf("%d bytes on the wire for a %d-byte push", n, len(text))
	}
}

func TestDescribeHintsAtDNSRebindProtection(t *testing.T) {
	notFound := &url.Error{Op: "Get", URL: "https://perch.example.com/api/v1/ap-agent/ws", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "perch.example.com", IsNotFound: true},
	}}
	if got := describe(notFound); !strings.Contains(got, "rebind_domain='perch.example.com'") {
		t.Fatalf("no rebind hint: %s", got)
	}
	timeout := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: "perch.example.com", IsTimeout: true}}
	if got := describe(timeout); strings.Contains(got, "rebind") {
		t.Fatalf("rebind hint on a timeout: %s", got)
	}
}
