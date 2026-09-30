package handlers

import (
	"context"

	"github.com/capthndsme/perch-agentkit/rpc"
	"github.com/capthndsme/perch-agentkit/update"
)

// Updater is agent self-update (internal/update over the kit's updater) as
// the handlers see it: system.info's update block, the capability, and the
// agent.update.* methods (agent-updates protocol.md sections 3 and 4).
type Updater interface {
	// Status is the update block, with fresh numbers.
	Status(ctx context.Context) update.Status
	// Capability is "agent_update" when this AP can update itself now.
	Capability() string
	// Register installs agent.update.*.
	Register(d *rpc.Dispatcher)
}

// registerUpdate installs the updater's methods.
func registerUpdate(disp *rpc.Dispatcher, u Updater) {
	if u != nil {
		u.Register(disp)
	}
}

// updateBlock is system.info's update block (nil without an updater).
func updateBlock(ctx context.Context, u Updater) *update.Status {
	if u == nil {
		return nil
	}
	st := u.Status(ctx)
	return &st
}
