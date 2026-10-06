package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The JSON API serializes DeployEvent directly, so RawPayload (full webhook
// bodies) must never appear in it.
func TestDeployEvent_JSONOmitsRawPayload(t *testing.T) {
	ev := DeployEvent{PRAuthor: "alice", RawPayload: json.RawMessage(`{"secret":"hunter2"}`)}

	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hunter2") || strings.Contains(string(out), "raw_payload") {
		t.Fatalf("serialized DeployEvent leaks the raw webhook payload: %s", out)
	}
}

func TestDeployEventID(t *testing.T) {
	gh := DeploySourceGitHubActions
	base := DeployEventID(gh, "acme/payments", "abc123")

	if base != DeployEventID(gh, "acme/payments", "abc123") {
		t.Error("ID is not deterministic")
	}
	if base != DeployEventID(gh, "Acme/Payments", "ABC123") {
		t.Error("ID should ignore case: GitHub repo names and SHAs are case-insensitive")
	}
	for name, other := range map[string]uuid.UUID{
		"other commit": DeployEventID(gh, "acme/payments", "def456"),
		"other repo":   DeployEventID(gh, "acme/billing", "abc123"),
		"other source": DeployEventID(DeploySourceGitLabCI, "acme/payments", "abc123"),
	} {
		if other == base {
			t.Errorf("%s collided with the base ID", name)
		}
	}
}

func TestBlameEdgeID(t *testing.T) {
	snap, deploy := uuid.New(), uuid.New()

	first, again := BlameEdgeID(snap, deploy), BlameEdgeID(snap, deploy)
	if first != again {
		t.Error("edge ID is not deterministic for the same (snapshot, deploy) pair")
	}
	if BlameEdgeID(snap, deploy) == BlameEdgeID(snap, uuid.New()) {
		t.Error("different deploys must give different edges")
	}
	if BlameEdgeID(snap, deploy) == BlameEdgeID(uuid.New(), deploy) {
		t.Error("different snapshots must give different edges")
	}
	if BlameEdgeID(snap, deploy) == BlameEdgeID(deploy, snap) {
		t.Error("the pair is ordered: (snapshot, deploy) is not (deploy, snapshot)")
	}
}

func TestParseBlameStatuses(t *testing.T) {
	got, err := ParseBlameStatuses(" Pending , resolved,,")
	if err != nil || len(got) != 2 || got[0] != BlameStatusPending || got[1] != BlameStatusResolved {
		t.Errorf("got %v, %v", got, err)
	}
	if all, err := ParseBlameStatuses("ALL"); err != nil || len(all) != 4 {
		t.Errorf("all = %v, %v", all, err)
	}
	// "active" (everything but dismissed) is shared by the UI and the API.
	active, err := ParseBlameStatuses(" Active ")
	if err != nil || len(active) != 3 {
		t.Fatalf("active = %v, %v", active, err)
	}
	for _, st := range active {
		if st == BlameStatusDismissed {
			t.Error("active must not include dismissed")
		}
	}
	if none, err := ParseBlameStatuses(""); err != nil || len(none) != 0 {
		t.Errorf("empty input means no filter, got %v, %v", none, err)
	}
	if _, err := ParseBlameStatuses("pending,bogus"); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("an unknown status must be rejected by name, got %v", err)
	}
}

func TestParseAnomalyStates(t *testing.T) {
	got, err := ParseAnomalyStates("candidates, unblamed")
	if err != nil || len(got) != 2 || got[0] != AnomalyStateCandidates || got[1] != AnomalyStateUnblamed {
		t.Errorf("got %v, %v", got, err)
	}
	if all, err := ParseAnomalyStates("all"); err != nil || len(all) != 5 {
		t.Errorf("all = %v, %v", all, err)
	}
	if _, err := ParseAnomalyStates("pending"); err == nil {
		t.Error("\"pending\" is an edge status, not an anomaly state, and must be rejected")
	}
}

func TestStateOf(t *testing.T) {
	for _, tc := range []struct {
		c    EdgeCounts
		want AnomalyState
	}{
		{EdgeCounts{}, AnomalyStateUnblamed},
		{EdgeCounts{Pending: 3}, AnomalyStateCandidates},
		{EdgeCounts{Pending: 1, Resolved: 1}, AnomalyStateResolved},
		{EdgeCounts{Resolved: 1, Confirmed: 1, Pending: 2, Dismissed: 1}, AnomalyStateConfirmed},
		{EdgeCounts{Dismissed: 2}, AnomalyStateDismissed},
		{EdgeCounts{Dismissed: 2, Pending: 1}, AnomalyStateCandidates}, // an unreviewed candidate still needs a look
	} {
		if got := StateOf(tc.c); got != tc.want {
			t.Errorf("StateOf(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
	if (EdgeCounts{Pending: 1, Resolved: 2, Confirmed: 3, Dismissed: 4}).Total() != 10 {
		t.Error("Total is the sum of the counts")
	}
}
