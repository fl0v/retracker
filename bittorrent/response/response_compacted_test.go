package response

import (
	"testing"

	"github.com/fl0v/retracker/bittorrent/common"
)

func TestCompactedIncludesIPv4AndIPv6Peers(t *testing.T) {
	peers := []common.Peer{
		{PeerID: common.PeerID("aaaaaaaaaaaaaaaaaaaa"), IP: common.Address("192.0.2.1"), Port: 6881},
		{PeerID: common.PeerID("bbbbbbbbbbbbbbbbbbbb"), IP: common.Address("2001:db8::1"), Port: 6882},
	}

	resp := Response{
		Interval:    30,
		MinInterval: 15,
		Complete:    1,
		Incomplete:  2,
		TrackerID:   "tracker-1",
		Peers:       peers,
	}

	compact := resp.Compacted()

	if got := len(compact.Peers4); got != 6 {
		t.Fatalf("unexpected peers4 length: %d", got)
	}
	if got := len(compact.Peers6); got != 18 {
		t.Fatalf("unexpected peers6 length: %d", got)
	}

	encoded, err := resp.Bencode(true)
	if err != nil {
		t.Fatalf("encode compacted: %v", err)
	}

	loaded, err := Load([]byte(encoded))
	if err != nil {
		t.Fatalf("decode compacted: %v", err)
	}

	if loaded.TrackerID != resp.TrackerID {
		t.Fatalf("tracker id mismatch: %q != %q", loaded.TrackerID, resp.TrackerID)
	}
	if loaded.MinInterval != resp.MinInterval {
		t.Fatalf("min interval mismatch: %d != %d", loaded.MinInterval, resp.MinInterval)
	}
	if len(loaded.Peers) != len(peers) {
		t.Fatalf("peer count mismatch: %d != %d", len(loaded.Peers), len(peers))
	}
	if loaded.Peers[0].IP != peers[0].IP || loaded.Peers[0].Port != peers[0].Port {
		t.Fatalf("ipv4 peer mismatch: %+v != %+v", loaded.Peers[0], peers[0])
	}
	if loaded.Peers[1].IP != peers[1].IP || loaded.Peers[1].Port != peers[1].Port {
		t.Fatalf("ipv6 peer mismatch: %+v != %+v", loaded.Peers[1], peers[1])
	}
}

func TestCompactedOmitsPeers6WhenEmpty(t *testing.T) {
	peers := []common.Peer{
		{PeerID: common.PeerID("cccccccccccccccccccc"), IP: common.Address("198.51.100.10"), Port: 51413},
	}

	resp := Response{
		Interval: 10,
		Peers:    peers,
	}

	compact := resp.Compacted()

	if compact.Peers6 != nil {
		t.Fatalf("expected peers6 to be nil when no IPv6 peers")
	}
	if got := len(compact.Peers4); got != 6 {
		t.Fatalf("unexpected peers4 length: %d", got)
	}
}
