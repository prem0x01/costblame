// Package notify defines the Notifier interface and a multi-notifier fan-out.
package notify

import (
	"context"

	"github.com/prem0x01/costblame/pkg/models"
)

// Notifier sends a blame graph alert to one destination (Slack, webhook, etc.).
type Notifier interface {
	Send(ctx context.Context, graph models.BlameGraph) error
}

// Multi fans out a blame graph to multiple notifiers.
// Errors from individual notifiers are logged but do not abort the others.
type Multi struct {
	notifiers []Notifier
}

// NewMulti creates a fan-out notifier from the provided list.
func NewMulti(nn ...Notifier) *Multi {
	return &Multi{notifiers: nn}
}

func (m *Multi) Send(ctx context.Context, graph models.BlameGraph) error {
	var lastErr error
	for _, n := range m.notifiers {
		if err := n.Send(ctx, graph); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Noop is a no-op notifier used when no alert destinations are configured.
type Noop struct{}

func (n *Noop) Send(_ context.Context, _ models.BlameGraph) error { return nil }
