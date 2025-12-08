package response

import (
	"testing"
)

// Tracker returned non-compact with empty peers list.
func TestLoadHandlesEmptyPeerList(t *testing.T) {
	payload := []byte("d8:intervali1800e5:peersdee")

	resp, err := Load(payload)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if resp.Interval != 1800 {
		t.Fatalf("interval mismatch: %d", resp.Interval)
	}
	if len(resp.Peers) != 0 {
		t.Fatalf("expected empty peers, got %d", len(resp.Peers))
	}
}
