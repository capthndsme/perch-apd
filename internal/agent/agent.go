// Package agent keeps the AP daemon connected to its controller: it joins
// with the join token once, then holds a WebSocket session open (one
// perch-agentkit link session at a time), serves the controller's JSON-RPC
// requests on it, pushes metrics on the controller's schedule, and
// reconnects with backoff (PROTOCOL.md).
package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/capthndsme/perch-agentkit/link"
	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-apd/internal/config"
	"github.com/capthndsme/perch-apd/internal/sysinfo"
	"github.com/capthndsme/perch-apd/internal/version"
)

// Subprotocol is the WebSocket subprotocol of protocol v1.
const Subprotocol = "perch-ap.v1"

// Controller endpoints.
const (
	JoinPath = "/api/v1/ap-agent/join"
	WSPath   = "/api/v1/ap-agent/ws"
)

// Timing.
const (
	slowRetry      = 5 * time.Minute
	replacedRetry  = 60 * time.Second
	readLimitBytes = 4 << 20

	// Metrics push (PROTOCOL.md §2.3): the controller sets the interval with
	// agent.configure right after connecting; these apply only if it never does.
	defaultPushInterval = 15 * time.Second
	defaultConfigWait   = 10 * time.Second
	collectTimeout      = 8 * time.Second
)

// DefaultCollectors is what the Perch Network Controller reads, pushed until
// the controller asks for a different set.
var DefaultCollectors = []string{"openwrt", "uname", "stat", "loadavg", "meminfo", "conntrack", "netdev", "wifi", "wifi_stations"}

// MetricsSource renders the Prometheus text of the named collectors.
type MetricsSource interface {
	Gather(ctx context.Context, names []string) []byte
}

// PushConfig is the schedule agent.configure sets.
type PushConfig struct {
	Interval   time.Duration // 0 = paused
	Collectors []string
}

// ParseConfigure reads agent.configure params.
func ParseConfigure(raw json.RawMessage) (PushConfig, error) {
	var p struct {
		MetricsIntervalSeconds *float64 `json:"metricsIntervalSeconds"`
		Collectors             []string `json:"collectors"`
	}
	if err := rpc.Params(raw, &p); err != nil {
		return PushConfig{}, err
	}
	cfg := PushConfig{Interval: defaultPushInterval, Collectors: DefaultCollectors}
	if p.MetricsIntervalSeconds != nil {
		cfg.Interval = link.IntervalFromSeconds(*p.MetricsIntervalSeconds)
	}
	if len(p.Collectors) > 0 {
		cfg.Collectors = p.Collectors
	}
	return cfg, nil
}

// JoinRequest is the body of POST /api/v1/ap-agent/join.
type JoinRequest struct {
	Token        string   `json:"token"`
	Hostname     string   `json:"hostname"`
	Model        string   `json:"model,omitempty"`
	BoardName    string   `json:"boardName,omitempty"`
	Release      string   `json:"release,omitempty"`
	Revision     string   `json:"revision,omitempty"`
	Target       string   `json:"target,omitempty"`
	Arch         string   `json:"arch,omitempty"`
	Kernel       string   `json:"kernel,omitempty"`
	AgentVersion string   `json:"agentVersion"`
	MACs         []string `json:"macs"`
}

// JoinResponse is `data` of a successful join.
type JoinResponse struct {
	AgentID     string `json:"agentId"`
	AgentSecret string `json:"agentSecret"`
	ApID        int    `json:"apId"`
	ApName      string `json:"apName"`
	Outcome     string `json:"outcome"`
}

// BuildJoinRequest describes this device for a join.
func BuildJoinRequest(ctx context.Context, info *sysinfo.Info, token string) JoinRequest {
	b := info.Board(ctx)
	macs := sysinfo.MACs()
	if macs == nil {
		macs = []string{}
	}
	hostname := b.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	return JoinRequest{
		Token:        token,
		Hostname:     truncate(hostname, 120),
		Model:        truncate(b.Model, 120),
		BoardName:    truncate(b.BoardName, 120),
		Release:      truncate(b.Release, 50),
		Revision:     truncate(b.Revision, 64),
		Target:       truncate(b.Target, 64),
		Arch:         version.Arch(),
		Kernel:       truncate(b.Kernel, 64),
		AgentVersion: truncate(version.Version, 32),
		MACs:         macs,
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Join trades a join token for agent credentials. A refusal is a
// *link.StatusError.
func Join(ctx context.Context, client *http.Client, controller string, req JoinRequest) (*JoinResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, link.Endpoint(controller, JoinPath), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", version.UserAgent())
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, link.ReadStatusError(resp)
	}
	var out struct {
		Data JoinResponse `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, fmt.Errorf("join: bad response: %w", err)
	}
	if out.Data.AgentID == "" || out.Data.AgentSecret == "" {
		return nil, errors.New("join: response without credentials")
	}
	return &out.Data, nil
}

// NewHTTPClient builds the client for join and the WebSocket handshake,
// honouring tls_insecure and ca_file.
func NewHTTPClient(cfg *config.Config) (*http.Client, error) {
	return link.NewHTTPClient(link.TLSOptions{Insecure: cfg.TLSInsecure, CAFile: cfg.CAFile})
}

// Options configure an Agent.
type Options struct {
	Config     *config.Config
	Log        *slog.Logger
	Dispatcher *rpc.Dispatcher
	Info       *sysinfo.Info
	HTTPClient *http.Client
	// Sleep waits d or until ctx is done (tests shorten it).
	Sleep func(ctx context.Context, d time.Duration) error
	// OnJoined is called after credentials were stored (tests).
	OnJoined func(*JoinResponse)
	// Metrics is pushed on the controller's schedule (nil = never push).
	Metrics MetricsSource
	// ConfigWait is how long to wait for agent.configure before pushing with
	// the defaults (tests shorten it).
	ConfigWait time.Duration
}

// Agent is the controller client.
type Agent struct {
	cfg   *config.Config
	log   *slog.Logger
	disp  *rpc.Dispatcher
	info  *sysinfo.Info
	http  *http.Client
	sleep func(ctx context.Context, d time.Duration) error
	onJ   func(*JoinResponse)
	mets  MetricsSource
	wait  time.Duration

	mu   sync.Mutex
	sess *link.Session
}

// New returns an agent.
func New(o Options) (*Agent, error) {
	client := o.HTTPClient
	if client == nil {
		var err error
		if client, err = NewHTTPClient(o.Config); err != nil {
			return nil, err
		}
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	wait := o.ConfigWait
	if wait <= 0 {
		wait = defaultConfigWait
	}
	disp := o.Dispatcher
	if disp == nil {
		disp = rpc.NewDispatcher()
	}
	return &Agent{cfg: o.Config, log: log, disp: disp, info: o.Info, http: client, sleep: sleep,
		onJ: o.OnJoined, mets: o.Metrics, wait: wait}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Connected reports whether a session is open.
func (a *Agent) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess != nil
}

// Notify sends a notification on the current session, if any.
func (a *Agent) Notify(method string, params any) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return
	}
	if err := s.Notify(method, params); err != nil {
		a.log.Debug("notification not sent", "method", method, "err", err)
	}
}

func (a *Agent) setSession(s *link.Session) {
	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()
}

// clearSession forgets s unless a newer session already replaced it.
func (a *Agent) clearSession(s *link.Session) {
	a.mu.Lock()
	if a.sess == s {
		a.sess = nil
	}
	a.mu.Unlock()
}

// Run keeps the agent joined and connected until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	bo := link.Backoff{Min: time.Second, Max: time.Minute}
	warnedNoToken := false
	for ctx.Err() == nil {
		if a.cfg.Controller == "" {
			a.log.Info("no controller configured; the WebSocket session is off")
			<-ctx.Done()
			return nil
		}
		if !a.cfg.HasCredentials() {
			if a.cfg.JoinToken == "" {
				if !warnedNoToken {
					a.log.Warn("not joined to the controller and no join_token is set; run: perch-apd join --controller <url> --token <token>")
					warnedNoToken = true
				}
				_ = a.sleep(ctx, slowRetry)
				continue
			}
			res, err := Join(ctx, a.http, a.cfg.Controller, BuildJoinRequest(ctx, a.info, a.cfg.JoinToken))
			if err != nil {
				wait := a.retryDelay(err, &bo)
				a.log.Warn("join failed", "controller", a.cfg.Controller, "err", describe(err), "retry_in", wait.Round(time.Second).String())
				_ = a.sleep(ctx, wait)
				continue
			}
			if err := a.cfg.SaveCredentials(res.AgentID, res.AgentSecret); err != nil {
				// Keep going with the credentials in memory; the next restart
				// would need the (now used) token again.
				a.log.Error("could not store the agent credentials", "path", a.cfg.Path, "err", err)
				a.cfg.AgentID, a.cfg.AgentSecret, a.cfg.JoinToken = res.AgentID, res.AgentSecret, ""
			}
			a.log.Info("joined the controller", "ap_id", res.ApID, "name", res.ApName, "outcome", res.Outcome)
			if a.onJ != nil {
				a.onJ(res)
			}
			bo.Reset()
		}

		started := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) > time.Minute {
			bo.Reset()
		}
		var se *link.StatusError
		code := websocket.CloseStatus(err)
		switch {
		case code == link.CloseRevoked || errors.As(err, &se) && se.Status == http.StatusUnauthorized:
			if a.cfg.JoinToken != "" {
				a.log.Warn("the controller rejected the agent credentials; joining again with the configured join_token")
				a.cfg.AgentID, a.cfg.AgentSecret = "", ""
				continue
			}
			a.log.Warn("the controller rejected the agent credentials (agent forgotten or deleted?); get a new join token and run: perch-apd join --token <token>")
			_ = a.sleep(ctx, slowRetry)
		case code == link.CloseReplaced:
			a.log.Warn("another agent connected with the same credentials; backing off", "retry_in", replacedRetry.String())
			_ = a.sleep(ctx, replacedRetry)
		case code == websocket.StatusGoingAway:
			a.log.Info("controller is restarting; reconnecting shortly")
			_ = a.sleep(ctx, 2*time.Second+time.Duration(rand.Intn(3000))*time.Millisecond)
		default:
			wait := a.retryDelay(err, &bo)
			a.log.Warn("controller session ended", "err", describe(err), "retry_in", wait.Round(time.Second).String())
			_ = a.sleep(ctx, wait)
		}
	}
	return nil
}

func (a *Agent) retryDelay(err error, bo *link.Backoff) time.Duration {
	var se *link.StatusError
	if errors.As(err, &se) {
		switch {
		case se.Status == http.StatusTooManyRequests && se.RetryAfter > 0:
			return se.RetryAfter
		case se.Status == http.StatusUnauthorized, se.Status == http.StatusUnprocessableEntity,
			se.Status == http.StatusNotFound, se.Status == http.StatusForbidden, se.Status == http.StatusBadRequest:
			return slowRetry
		}
	}
	return bo.Next()
}

func describe(err error) string {
	if err == nil {
		return "closed"
	}
	var se *link.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusNotFound:
			return se.Error() + " (the controller has no AP daemon support: update Perch Network Controller, or check the URL)"
		case http.StatusUnauthorized:
			if se.Code == "invalid_join_token" {
				return se.Error() + " (create a new join token in Settings → Wi-Fi sources)"
			}
		case http.StatusBadRequest:
			if se.Code == "unsupported_protocol" {
				return se.Error() + " (the daemon and the controller speak different protocol versions: update the older one)"
			}
		}
		return se.Error()
	}
	var cerr websocket.CloseError
	if errors.As(err, &cerr) {
		return fmt.Sprintf("closed by the controller: %d %s", cerr.Code, cerr.Reason)
	}
	var uerr x509.UnknownAuthorityError
	if errors.As(err, &uerr) {
		return err.Error() + " (install ca-bundle, set ca_file, or tls_insecure '1' for a self-signed certificate)"
	}
	return err.Error()
}

// session holds one link session: agent.configure feeds the push schedule
// (the collectors it names apply from the next push), the pusher runs for
// as long as the session does.
func (a *Agent) session(ctx context.Context) error {
	wsURL, err := link.WebSocketURL(a.cfg.Controller, WSPath)
	if err != nil {
		return err
	}
	configs := make(chan link.Schedule, 1)
	var collMu sync.Mutex
	collectors := DefaultCollectors

	return link.Run(ctx, link.Options{
		URL:         wsURL,
		Subprotocol: Subprotocol,
		Header: http.Header{
			"Authorization": {"Bearer " + a.cfg.AgentID + "." + a.cfg.AgentSecret},
			"User-Agent":    {version.UserAgent()},
		},
		HTTPClient: a.http,
		ReadLimit:  readLimitBytes,
		Log:        a.log,
		Dispatcher: a.disp,
		OnNotification: func(_ context.Context, _ *link.Session, m *rpc.Message) {
			if m.Method != "agent.configure" {
				a.log.Debug("notification from the controller", "method", m.Method)
				return
			}
			cfg, err := ParseConfigure(m.Params)
			if err != nil {
				a.log.Warn("bad agent.configure from the controller", "err", err)
				return
			}
			a.log.Info("metrics schedule from the controller", "interval", cfg.Interval.String(), "collectors", len(cfg.Collectors))
			collMu.Lock()
			collectors = cfg.Collectors
			collMu.Unlock()
			link.Offer(configs, link.Schedule{Interval: cfg.Interval})
		},
		OnOpen: func(sctx context.Context, s *link.Session) {
			a.setSession(s)
			defer a.clearSession(s)
			a.log.Info("connected to the controller", "url", wsURL)
			if a.mets == nil {
				<-sctx.Done()
				return
			}
			link.RunPusher(sctx, link.PushOptions{
				Configs:      configs,
				Fallback:     &link.Schedule{Interval: defaultPushInterval},
				FallbackWait: a.wait,
				Log:          a.log,
				Push: func(pctx context.Context, seq uint64) {
					collMu.Lock()
					names := collectors
					collMu.Unlock()
					start := time.Now()
					gctx, cancel := context.WithTimeout(pctx, collectTimeout)
					text := a.mets.Gather(gctx, names)
					cancel()
					err := s.Notify("metrics.push", map[string]any{
						"format":      "prometheus-text",
						"text":        string(text),
						"collectedAt": start.UTC().Format(time.RFC3339),
						"durationMs":  time.Since(start).Milliseconds(),
						"seq":         seq,
					})
					if err != nil && pctx.Err() == nil {
						a.log.Debug("metrics push not sent", "err", err)
					}
				},
			})
		},
	})
}
