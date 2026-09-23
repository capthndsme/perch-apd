package agent

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/capthndsme/perch-agentkit/link"
)

func TestRetryDelay(t *testing.T) {
	a := &Agent{}
	cases := []struct {
		name     string
		err      error
		min, max time.Duration
	}{
		{"503 gateway starting", &link.StatusError{Status: http.StatusServiceUnavailable, RetryAfter: time.Second}, time.Second, time.Second},
		{"429 retry-after", &link.StatusError{Status: http.StatusTooManyRequests, RetryAfter: 7 * time.Second}, 7 * time.Second, 7 * time.Second},
		{"401", &link.StatusError{Status: http.StatusUnauthorized}, slowRetry, slowRetry},
		{"503 without retry-after", &link.StatusError{Status: http.StatusServiceUnavailable}, 900 * time.Millisecond, 1100 * time.Millisecond},
		{"503 with a long retry-after", &link.StatusError{Status: http.StatusServiceUnavailable, RetryAfter: time.Hour}, 900 * time.Millisecond, 1100 * time.Millisecond},
		{"network", errors.New("connection refused"), 900 * time.Millisecond, 1100 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bo := link.Backoff{Min: time.Second, Max: reconnectMax}
			got := a.retryDelay(tc.err, &bo)
			if got < tc.min || got > tc.max {
				t.Fatalf("wait %s, want %s..%s", got, tc.min, tc.max)
			}
		})
	}
}

// A controller that stays down is retried at least about every 30 s.
func TestReconnectLadderIsCapped(t *testing.T) {
	a := &Agent{}
	bo := link.Backoff{Min: time.Second, Max: reconnectMax}
	var last time.Duration
	for i := 0; i < 12; i++ {
		last = a.retryDelay(errors.New("connection refused"), &bo)
		if last > reconnectMax+reconnectMax/10 {
			t.Fatalf("attempt %d waits %s, over the %s cap", i+1, last, reconnectMax)
		}
	}
	if last < reconnectMax-reconnectMax/10 {
		t.Fatalf("the ladder never reached its cap: %s", last)
	}
}
