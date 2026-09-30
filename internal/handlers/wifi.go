package handlers

import (
	"context"
	"encoding/json"

	"github.com/capthndsme/perch-agentkit/rpc"
)

// WifiPlane is the Wi-Fi config plane (internal/wifiplane) as the handlers
// see it: its block of system.info and its wifi.* methods. nil = a build
// without it.
type WifiPlane interface {
	// Hello is system.info's wifiConfig block for the session of ctx.
	Hello(ctx context.Context) any
	// Methods are the wifi.* methods it serves.
	Methods() []string
	// Serve runs one of them for the session of ctx.
	Serve(ctx context.Context, method string, params json.RawMessage) (any, error)
}

// registerWifi installs the plane's methods.
func registerWifi(disp *rpc.Dispatcher, w WifiPlane) {
	if w == nil {
		return
	}
	for _, method := range w.Methods() {
		method := method
		disp.Register(method, func(ctx context.Context, raw json.RawMessage) (any, error) {
			return w.Serve(ctx, method, raw)
		})
	}
}
