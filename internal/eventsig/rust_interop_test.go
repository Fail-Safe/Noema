package eventsig

import (
	"encoding/json"
	"testing"

	"github.com/Fail-Safe/Noema/internal/event"
)

// TestRustWireFixtureVerifies locks the Rust comparison emitter to the Go
// signing wire contract. The fixture is a public key and signed event produced
// by noema-rs, then serialized in the same shape returned by sync_events.
func TestRustWireFixtureVerifies(t *testing.T) {
	const wire = `{
  "id":"01M01NJPRMG2F25DEGFG3BEZKR",
  "action":"create",
  "trace_id":"20260815-network-rust-create",
  "cortex_id":"01M01NJPQ0A6W5Z2JC3V867G0G",
  "origin":"rust-source",
  "timestamp":"2026-08-15T02:56:22Z",
  "data":{"title":"network-rust-create","type":"fact","author":"test","tags":["network"],"origin":"rust-source","tier":"short","body":"created by the Rust implementation","content_hash":"sha256:4ab6c6ce318973fb3f99cd552ad4bc8eda13e8cf4fbf307fdb5bf045b4ac3bf1"},
  "vclock":{"01M01NJPQ0A6W5Z2JC3V867G0G":1},
  "signature":"ed25519:1nXkQ+QwIPuEkaTqkdk9A2tSm4MwOwjW4gkEh4trKF1VRf8bRfyfFyEU1Xrw5T2/+ewtC5T/Ab7TREZk9qRAAQ==",
  "pubkey":"ed25519:LB+0jMLPWjc+6yXzZFm8KOGQCwjA2YzdGgbNEfcDCJo="
}`
	var e event.Event
	if err := json.Unmarshal([]byte(wire), &e); err != nil {
		t.Fatal(err)
	}
	if err := Verify(e.PubKey, e, e.Signature); err != nil {
		t.Fatalf("Rust wire signature does not satisfy the Go verifier: %v", err)
	}
}
