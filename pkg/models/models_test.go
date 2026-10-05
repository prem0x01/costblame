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

	if BlameEdgeID(snap, deploy) != BlameEdgeID(snap, deploy) {
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
