package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

var mergeBase = time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)

func mergeEvent() models.DeployEvent {
	return models.DeployEvent{
		ID:          models.DeployEventID(models.DeploySourceGitHubActions, "acme/payments", "abc123"),
		OccurredAt:  mergeBase,
		Source:      models.DeploySourceGitHubActions,
		Repository:  "acme/payments",
		Branch:      "main",
		CommitSHA:   "abc123",
		Environment: "prod",
		Status:      models.DeployStatusSuccess,
	}
}

func enriched() models.DeployEvent {
	ev := mergeEvent()
	ev.PRNumber = 42
	ev.PRTitle = "feat: provisioned concurrency"
	ev.PRAuthor = "alice"
	ev.PRLabels = []string{"cost"}
	ev.ChangedFiles = []string{"terraform/lambda/main.tf"}
	ev.InferredServices = []string{"AWSLambda"}
	return ev
}

// load returns the single stored deploy; it also asserts upserts never created a second row.
func load(t *testing.T, s interface {
	DeploysBetween(context.Context, time.Time, time.Time) ([]models.DeployEvent, error)
}) models.DeployEvent {
	t.Helper()
	evs, err := s.DeploysBetween(context.Background(), mergeBase.Add(-30*24*time.Hour), mergeBase.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("stored deploys = %d, want exactly 1 (same ID must upsert, not insert)", len(evs))
	}
	return evs[0]
}

// The scenario that used to wipe data: two workflows finish for one commit.
// The first event is enriched; the second arrives raw and must not erase it.
func TestUpsertDeploy_UnenrichedEventDoesNotWipeEnrichment(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDeployEvent(ctx, enriched()); err != nil {
		t.Fatal(err)
	}
	// Raw event: no PR info, nil slices (which json.Marshal would encode as "null").
	if err := store.UpsertDeployEvent(ctx, mergeEvent()); err != nil {
		t.Fatal(err)
	}

	got := load(t, store)
	if got.PRNumber != 42 || got.PRTitle != "feat: provisioned concurrency" || got.PRAuthor != "alice" {
		t.Errorf("PR fields wiped: %+v", got)
	}
	if len(got.PRLabels) != 1 || len(got.ChangedFiles) != 1 || len(got.InferredServices) != 1 {
		t.Errorf("array fields wiped: labels=%v files=%v services=%v", got.PRLabels, got.ChangedFiles, got.InferredServices)
	}
}

func TestUpsertDeploy_ArrivalOrderDoesNotMatter(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Raw first, enriched second — the original two-phase flow.
	if err := store.UpsertDeployEvent(ctx, mergeEvent()); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDeployEvent(ctx, enriched()); err != nil {
		t.Fatal(err)
	}
	got := load(t, store)
	if got.PRNumber != 42 || got.PRAuthor != "alice" || len(got.InferredServices) != 1 {
		t.Errorf("enrichment not applied: %+v", got)
	}
}

func TestUpsertDeploy_NeverOverwritesExistingValues(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDeployEvent(ctx, enriched()); err != nil {
		t.Fatal(err)
	}
	other := enriched()
	other.PRNumber, other.PRTitle, other.PRAuthor = 99, "something else", "mallory"
	other.ChangedFiles = []string{"other.go"}
	if err := store.UpsertDeployEvent(ctx, other); err != nil {
		t.Fatal(err)
	}

	got := load(t, store)
	if got.PRNumber != 42 || got.PRAuthor != "alice" || got.ChangedFiles[0] != "terraform/lambda/main.tf" {
		t.Errorf("existing values were overwritten: %+v", got)
	}
}

// occurred_at keeps the EARLIEST time. A scheduled run on an old commit
// finishing later must not make that commit look freshly deployed.
func TestUpsertDeploy_OccurredAtKeepsEarliest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.UpsertDeployEvent(ctx, mergeEvent()); err != nil {
		t.Fatal(err)
	}
	later := mergeEvent()
	later.OccurredAt = mergeBase.Add(72 * time.Hour)
	if err := store.UpsertDeployEvent(ctx, later); err != nil {
		t.Fatal(err)
	}
	if got := load(t, store); !got.OccurredAt.Equal(mergeBase) {
		t.Errorf("OccurredAt = %v, want the earliest %v", got.OccurredAt, mergeBase)
	}

	earlier := mergeEvent()
	earlier.OccurredAt = mergeBase.Add(-time.Hour)
	if err := store.UpsertDeployEvent(ctx, earlier); err != nil {
		t.Fatal(err)
	}
	if got := load(t, store); !got.OccurredAt.Equal(mergeBase.Add(-time.Hour)) {
		t.Errorf("OccurredAt = %v, want the new earliest %v", got.OccurredAt, mergeBase.Add(-time.Hour))
	}
}

// Sources may report non-UTC offsets; they must still compare correctly.
func TestUpsertDeploy_OccurredAtComparesAcrossTimezones(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ist := time.FixedZone("IST", 5*3600+1800)
	first := mergeEvent()
	first.OccurredAt = mergeBase.In(ist) // 15:30 IST == 10:00 UTC
	if err := store.UpsertDeployEvent(ctx, first); err != nil {
		t.Fatal(err)
	}
	later := mergeEvent()
	later.OccurredAt = mergeBase.Add(time.Hour) // 11:00 UTC; as text "11:00" sorts before "15:30"
	if err := store.UpsertDeployEvent(ctx, later); err != nil {
		t.Fatal(err)
	}
	if got := load(t, store); !got.OccurredAt.Equal(mergeBase) {
		t.Errorf("OccurredAt = %v, want %v (timezone offsets must not skew the comparison)", got.OccurredAt, mergeBase)
	}
}

func TestUpsertDeploy_EnvironmentUnknownIsUpgradedButRealValuesKept(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	unknown := mergeEvent()
	unknown.Environment = "unknown"
	if err := store.UpsertDeployEvent(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDeployEvent(ctx, mergeEvent()); err != nil { // prod
		t.Fatal(err)
	}
	if got := load(t, store); got.Environment != "prod" {
		t.Errorf("environment = %q, want unknown upgraded to prod", got.Environment)
	}

	staging := mergeEvent()
	staging.Environment = "staging"
	if err := store.UpsertDeployEvent(ctx, staging); err != nil {
		t.Fatal(err)
	}
	if got := load(t, store); got.Environment != "prod" {
		t.Errorf("environment = %q, want prod kept (never overwritten)", got.Environment)
	}
}

func TestUpsertDeploy_NilSlicesAreStoredAsEmptyArrays(t *testing.T) {
	store := newTestStore(t)
	if err := store.UpsertDeployEvent(context.Background(), mergeEvent()); err != nil {
		t.Fatal(err)
	}
	got := load(t, store)
	if got.PRLabels == nil || got.ChangedFiles == nil || got.InferredServices == nil {
		t.Errorf("nil slices should round-trip as empty arrays, got %v %v %v", got.PRLabels, got.ChangedFiles, got.InferredServices)
	}
}

// Services come from different evidence at different times (a repository rule
// at ingestion, changed files after enrichment). They must combine.
func TestUpsertDeploy_InferredServicesAreUnioned(t *testing.T) {
	cases := []struct {
		name          string
		first, second []string
		want          []string
	}{
		{"enrichment adds to a repo rule", []string{"lambda"}, []string{"AmazonS3", "AWSLambda"}, []string{"AWSLambda", "AmazonS3", "lambda"}},
		{"a raw event does not erase them", []string{"AWSLambda"}, nil, []string{"AWSLambda"}},
		{"first event empty", nil, []string{"AmazonS3"}, []string{"AmazonS3"}},
		{"identical sets stay the same", []string{"AWSLambda"}, []string{"AWSLambda"}, []string{"AWSLambda"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()

			a := mergeEvent()
			a.InferredServices = tc.first
			if err := store.UpsertDeployEvent(ctx, a); err != nil {
				t.Fatal(err)
			}
			b := mergeEvent()
			b.InferredServices = tc.second
			if err := store.UpsertDeployEvent(ctx, b); err != nil {
				t.Fatal(err)
			}

			got := load(t, store).InferredServices
			if len(got) != len(tc.want) {
				t.Fatalf("services = %v, want %v", got, tc.want)
			}
			have := map[string]bool{}
			for _, g := range got {
				have[g] = true
			}
			for _, w := range tc.want {
				if !have[w] {
					t.Errorf("services = %v, missing %q", got, w)
				}
			}
		})
	}
}
