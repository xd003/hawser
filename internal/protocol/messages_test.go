package protocol

import (
	"encoding/json"
	"testing"
)

// Dockhand's own rejections use "message"; agent-originated errors use "error".
// Both must reach the operator's log.
func TestErrorMessageReasonAcceptsDockhandAndAgentShapes(t *testing.T) {
	for raw, want := range map[string]string{
		`{"type":"error","message":"Docker instance already registered as \"std\""}`: `Docker instance already registered as "std"`,
		`{"type":"error","error":"request failed","requestId":"r1"}`:                 "request failed",
	} {
		var msg ErrorMessage
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatal(err)
		}
		if got := msg.Reason(); got != want {
			t.Fatalf("Reason() = %q, want %q", got, want)
		}
	}
}
