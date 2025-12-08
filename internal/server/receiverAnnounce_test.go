package server

import (
	"strings"
	"testing"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
)

func TestProcessAnnounceRespectsNumwantAndSetsFields(t *testing.T) {
	cfg := &config.Config{
		AnnounceResponseInterval: 30,
		MinAnnounceInterval:      10,
		TrackerID:                "tracker-1",
	}

	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	ra := &ReceiverAnnounce{
		Config:  cfg,
		Storage: storage,
	}

	infoHash := common.InfoHash("12345678901234567890")

	seedReq, err := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", "started", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("seed make request: %v", err)
	}
	leecherReq, err := tracker.MakeRequest("203.0.113.11", string(infoHash), strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("leecher make request: %v", err)
	}

	storage.Update(*seedReq)
	storage.Update(*leecherReq)

	requesterID := common.PeerID(strings.Repeat("c", 20))

	resp, failure := ra.ProcessAnnounce("203.0.113.30", string(infoHash), string(requesterID), "60000", "0", "0", "100", "", "1", "started", "qbittorrent", "", "")
	if failure != "" {
		t.Fatalf("unexpected failure: %s", failure)
	}
	if resp == nil {
		t.Fatalf("nil response")
	}

	if resp.MinInterval != cfg.MinAnnounceInterval {
		t.Fatalf("min interval mismatch: %d", resp.MinInterval)
	}
	if resp.Interval != cfg.AnnounceResponseInterval {
		t.Fatalf("interval mismatch: %d", resp.Interval)
	}
	if resp.TrackerID != cfg.TrackerID {
		t.Fatalf("tracker id mismatch: %q", resp.TrackerID)
	}
	if resp.Complete != 1 || resp.Incomplete != 2 {
		t.Fatalf("unexpected counts complete=%d incomplete=%d", resp.Complete, resp.Incomplete)
	}

	if len(resp.Peers) != 1 {
		t.Fatalf("expected 1 peer due to numwant, got %d", len(resp.Peers))
	}
	if resp.Peers[0].PeerID == requesterID {
		t.Fatalf("response should not include requester")
	}
}
