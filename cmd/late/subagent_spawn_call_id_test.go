package main

import (
	"encoding/json"
	"testing"

	"late/internal/client"
	"late/internal/session"
)

// spawnCallRecord builds a manifest record with the correlation surface
// needed by matchSpawnRecord.
func spawnCallRecord(id, agentType, goal, spawnCallID string) session.SubagentRecord {
	return session.SubagentRecord{
		ID:          id,
		AgentType:   agentType,
		Goal:        goal,
		Status:      session.SubagentStatusRunning,
		SpawnCallID: spawnCallID,
	}
}

// spawnCallTC builds a dangling spawn_subagent tool call whose arguments and
// tool-call ID are under test control.
func spawnCallTC(id, agentType, goal string) client.ToolCall {
	args, err := json.Marshal(spawnArgs{Goal: goal, AgentType: agentType})
	if err != nil {
		panic(err)
	}
	return client.ToolCall{ID: id, Function: client.FunctionCall{Name: "spawn_subagent", Arguments: string(args)}}
}

// TestMatchSpawnRecord_ExactCallIDBinding: two identical (type, goal) spawns
// with distinct tool-call IDs each bind to their OWN record via the exact
// spawn_call_id tier — the descend-out-of-duplicates ambiguity is gone.
func TestMatchSpawnRecord_ExactCallIDBinding(t *testing.T) {
	manifest := &session.SubagentManifest{Records: []session.SubagentRecord{
		spawnCallRecord("child-a", "coder", "same goal", "call-a"),
		spawnCallRecord("child-b", "coder", "same goal", "call-b"),
	}}
	consumed := make(map[*session.SubagentRecord]bool)

	if got := matchSpawnRecord(manifest, spawnCallTC("call-a", "coder", "same goal"), consumed); got == nil || got.ID != "child-a" {
		t.Errorf("call-a bound to %v, want child-a", recID(got))
	}
	if got := matchSpawnRecord(manifest, spawnCallTC("call-b", "coder", "same goal"), consumed); got == nil || got.ID != "child-b" {
		t.Errorf("call-b bound to %v, want child-b", recID(got))
	}
}

// TestMatchSpawnRecord_ExactTierIndependentOfManifestOrder: the exact
// binding does not depend on record ordering (the positional descend
// heuristic does).
func TestMatchSpawnRecord_ExactTierIndependentOfManifestOrder(t *testing.T) {
	manifest := &session.SubagentManifest{Records: []session.SubagentRecord{
		spawnCallRecord("child-b", "coder", "same goal", "call-b"),
		spawnCallRecord("child-a", "coder", "same goal", "call-a"),
	}}
	consumed := make(map[*session.SubagentRecord]bool)

	if got := matchSpawnRecord(manifest, spawnCallTC("call-a", "coder", "same goal"), consumed); got == nil || got.ID != "child-a" {
		t.Errorf("call-a bound to %v, want child-a", recID(got))
	}
	if got := matchSpawnRecord(manifest, spawnCallTC("call-b", "coder", "same goal"), consumed); got == nil || got.ID != "child-b" {
		t.Errorf("call-b bound to %v, want child-b", recID(got))
	}
}

// TestMatchSpawnRecord_LegacyRecordStillRestoresViaDescend: an empty
// spawn_call_id (manifests written before the field existed) matches by the
// pre-existing descend heuristic — no behavior change.
func TestMatchSpawnRecord_LegacyRecordStillRestoresViaDescend(t *testing.T) {
	manifest := &session.SubagentManifest{Records: []session.SubagentRecord{
		spawnCallRecord("child-legacy", "coder", "legacy goal", ""),
	}}
	consumed := make(map[*session.SubagentRecord]bool)

	got := matchSpawnRecord(manifest, spawnCallTC("call-x", "coder", "legacy goal"), consumed)
	if got == nil || got.ID != "child-legacy" {
		t.Fatalf("legacy record bound to %v, want child-legacy", recID(got))
	}
}

// TestMatchSpawnRecord_MismatchedCallIDFallsBackToDescend: a record bound to
// a DIFFERENT tool-call ID never matches the exact tier — it stays eligible
// for the descend path, exactly as a legacy record would.
func TestMatchSpawnRecord_MismatchedCallIDFallsBackToDescend(t *testing.T) {
	manifest := &session.SubagentManifest{Records: []session.SubagentRecord{
		spawnCallRecord("child-bound", "coder", "goal g", "call-other"),
	}}
	consumed := make(map[*session.SubagentRecord]bool)

	got := matchSpawnRecord(manifest, spawnCallTC("call-mine", "coder", "goal g"), consumed)
	if got == nil || got.ID != "child-bound" {
		t.Fatalf("mismatched spawn_call_id must fall back to descend, got %v", recID(got))
	}
}

// TestMatchSpawnRecord_ExactWinsOverDescendMatch: when both a call-ID-bound
// record and an argument-matching legacy record could serve a dangling call
// with a matching ID, the exact binding wins (the record claims only its own
// call, and a record bound to the call's ID is unambiguous).
func TestMatchSpawnRecord_ExactWinsOverDescendMatch(t *testing.T) {
	manifest := &session.SubagentManifest{Records: []session.SubagentRecord{
		spawnCallRecord("child-legacy", "coder", "same goal", ""),
		spawnCallRecord("child-exact", "coder", "same goal", "call-a"),
	}}
	consumed := make(map[*session.SubagentRecord]bool)

	if got := matchSpawnRecord(manifest, spawnCallTC("call-a", "coder", "same goal"), consumed); got == nil || got.ID != "child-exact" {
		t.Errorf("exact binding lost to the descend tier: got %v", recID(got))
	}
}

// recID nil-safely renders the matched record's ID for error messages.
func recID(rec *session.SubagentRecord) string {
	if rec == nil {
		return "<nil>"
	}
	return rec.ID
}
