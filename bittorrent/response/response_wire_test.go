package response_test

import (
	"strings"
	"testing"

	"github.com/zeebo/bencode"

	"github.com/fl0v/retracker/bittorrent/response"
)

func TestFailureResponseRoundTrip(t *testing.T) {
	for _, compact := range []bool{false, true} {
		original := response.NewFailure("tracker busy")
		original.Interval = 300
		original.MinInterval = 300
		original.RetryIn = 5
		encoded, err := original.Bencode(compact)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := response.Load([]byte(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if decoded.FailureReason != "tracker busy" || decoded.Interval != 300 || decoded.MinInterval != 300 || decoded.RetryIn != int64(5) {
			t.Fatalf("failure metadata did not survive compact=%t: %+v", compact, decoded)
		}
	}
}

func TestTolerantPeerListDecodingPreservesUsablePeers(t *testing.T) {
	firstID := strings.Repeat("p", 20)
	secondID := strings.Repeat("q", 20)
	payload, err := bencode.EncodeBytes(map[string]interface{}{
		"interval":     1800,
		"min interval": 900,
		"tracker id":   "upstream",
		"retry in":     "never",
		"peers": []interface{}{
			map[string]interface{}{"ip": "192.0.2.1", "port": 6881, "peer id": firstID},
			"invalid list entry",
			int64(123),
			map[string]interface{}{},
			map[string]interface{}{"port": 6882},
			map[string]interface{}{"ip": "2001:db8::1", "port": 6883, "peer_id": secondID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := response.Load(payload)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Interval != 1800 || decoded.MinInterval != 900 || decoded.TrackerID != "upstream" || decoded.RetryIn != "never" {
		t.Fatalf("tracker metadata was lost: %+v", decoded)
	}
	if len(decoded.Peers) != 3 {
		t.Fatalf("expected three usable entries, got %+v", decoded.Peers)
	}
	first, partial, last := decoded.Peers[0], decoded.Peers[1], decoded.Peers[2]
	if first.IP != "192.0.2.1" || first.Port != 6881 || string(first.PeerID) != firstID {
		t.Fatalf("first peer was not decoded: %+v", first)
	}
	if partial.Port != 6882 || partial.IP != "" {
		t.Fatalf("partial peer was not preserved: %+v", partial)
	}
	if last.IP != "2001:db8::1" || last.Port != 6883 || string(last.PeerID) != secondID {
		t.Fatalf("IPv6 peer was not decoded: %+v", last)
	}
}

func TestMalformedBencodeResponseReturnsError(t *testing.T) {
	for _, payload := range []string{"", "not bencode", "d8:intervali1800e", "d5:peers10:abce"} {
		if decoded, err := response.Load([]byte(payload)); err == nil || decoded != nil {
			t.Fatalf("invalid payload %q returned response=%+v error=%v", payload, decoded, err)
		}
	}
}
