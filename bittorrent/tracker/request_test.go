package tracker_test

import (
	"strings"
	"testing"

	"github.com/fl0v/retracker/bittorrent/tracker"
)

func TestMakeRequestRejectsMalformedRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index int
		value string
		field string
	}{
		{"short hash", 0, "short", "info_hash"},
		{"long hash", 0, strings.Repeat("h", 21), "info_hash"},
		{"short peer", 1, "short", "peer_id"},
		{"non-numeric port", 2, "invalid", "port"},
		{"negative uploaded", 3, "-1", "uploaded"},
		{"overflow downloaded", 4, "18446744073709551616", "downloaded"},
		{"non-numeric left", 5, "invalid", "left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := []string{strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "1"}
			values[tc.index] = tc.value
			req, err := tracker.MakeRequest("192.0.2.1", values[0], values[1], values[2], values[3], values[4], values[5], "", "", "started", "", "", "", nil)
			if err == nil || req != nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("got request=%v error=%v, want rejection for %s", req, err, tc.field)
			}
		})
	}
}

func TestMakeRequestLimitsPeerCount(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  uint64
	}{
		{"", 50},
		{"0", 50},
		{"-1", 50},
		{"invalid", 50},
		{"1", 1},
		{"200", 200},
		{"201", 200},
		{"18446744073709551615", 200},
	} {
		t.Run(tc.value, func(t *testing.T) {
			req, err := tracker.MakeRequest("192.0.2.1", strings.Repeat("h", 20), strings.Repeat("p", 20), "6881", "0", "0", "1", "", tc.value, "", "", "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if req.NumWant != tc.want {
				t.Fatalf("got numwant=%d, want %d", req.NumWant, tc.want)
			}
		})
	}
}

func TestMakeRequestPreservesBinaryIdentifiersAndPeerAddress(t *testing.T) {
	hash := "\x00\xff" + strings.Repeat("h", 18)
	peerID := "\x00\xfe" + strings.Repeat("p", 18)
	for _, tc := range []struct{ ip, want string }{
		{"", "2001:db8::1"},
		{"192.0.2.2", "192.0.2.2"},
	} {
		req, err := tracker.MakeRequest("2001:db8::1", hash, peerID, "6881", "12", "34", "56", tc.ip, "50", "completed", "test-client", "1", "1", nil)
		if err != nil {
			t.Fatal(err)
		}
		peer := req.Peer()
		if string(req.InfoHash) != hash || string(peer.PeerID) != peerID || string(peer.IP) != tc.want || peer.Port != 6881 {
			t.Fatalf("binary identifiers or peer address changed: %+v", req)
		}
		if req.Uploaded != 12 || req.Downloaded != 34 || req.Left != 56 || !req.Compact || !req.NoPeerID || req.Event != "completed" || req.UserAgent != "test-client" {
			t.Fatalf("announce fields were not preserved: %+v", req)
		}
	}
}
