// Package collect defines the interfaces that all cost and deploy event sources
// must implement. The correlation engine depends only on these interfaces,
// keeping it decoupled from any specific cloud provider or CI/CD system.
package collect

import (
	"context"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

// CostSource fetches cloud billing data for a time range.
// Implementations exist for AWS Cost Explorer and (planned) GCP Billing.
type CostSource interface {
	// Name returns the short identifier used in CostSnapshot.Source, e.g. "aws".
	Name() string

	// Collect returns all cost snapshots whose billing period overlaps [from, to].
	// Implementations must set IsAnomaly and AnomalyScore before returning.
	Collect(ctx context.Context, from, to time.Time) ([]models.CostSnapshot, error)
}

// DeploySource provides a stream of deployment events.
// Implementations are either webhook receivers (push model) or API pollers (pull model).
type DeploySource interface {
	// Name returns the short identifier used in DeployEvent.Source, e.g. "github_actions".
	Name() string

	// Events returns a channel that emits DeployEvents as they arrive.
	// The channel is closed when ctx is cancelled.
	// Implementations that poll an API should do so on a ticker inside this method.
	Events(ctx context.Context) (<-chan models.DeployEvent, error)
}
