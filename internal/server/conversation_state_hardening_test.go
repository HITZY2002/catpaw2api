package server

import (
	"testing"

	"catpaw2api/internal/pool"
)

func TestFinalizeConversationUsesCopyOnWrite(t *testing.T) {
	h := newTestHandler()
	acct := &pool.Account{Name: "a"}
	original := &convState{
		ChatID:         "chat-1",
		ConversationID: "conv-1",
		Account:        "a",
		Absorbed:       1,
		Fingerprint:    "old",
	}
	h.convs[original.ConversationID] = original
	h.latest[acct.Name] = original.ConversationID

	req := &chatRequest{Messages: userMsgs("q1")}
	h.finalizeConversation(acct, original, req, "a1", nil)

	if original.Absorbed != 1 || original.Fingerprint != "old" {
		t.Fatalf("input snapshot was mutated: %+v", original)
	}
	stored := h.convs[original.ConversationID]
	if stored == nil {
		t.Fatal("stored conversation missing")
	}
	if stored == original {
		t.Fatal("stored state must be a new snapshot")
	}
	if stored.Absorbed != 2 || stored.Fingerprint == "" || stored.Fingerprint == "old" {
		t.Fatalf("stored snapshot not updated: %+v", stored)
	}
}

func TestPlanConversationReturnsSnapshot(t *testing.T) {
	h := newTestHandler()
	stored := &convState{
		ChatID:         "chat-1",
		ConversationID: "conv-1",
		Account:        "a",
		Absorbed:       1,
		Fingerprint:    fingerprintOf(userMsgs("q1")),
	}
	h.convs[stored.ConversationID] = stored
	h.latest["a"] = stored.ConversationID

	acct := &pool.Account{Name: "a"}
	req := &chatRequest{Messages: userMsgs("q1", "q2")}
	got, isNew, prompt, err := h.planConversation(acct, req, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if isNew || got == nil || prompt != "q2" {
		t.Fatalf("got=%+v isNew=%v prompt=%q", got, isNew, prompt)
	}
	if got == stored {
		t.Fatal("planConversation must not expose the stored mutable pointer")
	}
	got.Fingerprint = "changed-locally"
	if h.convs[stored.ConversationID].Fingerprint == "changed-locally" {
		t.Fatal("mutating request snapshot leaked back into stored state")
	}
}
