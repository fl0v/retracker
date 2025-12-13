package server

import (
	"strings"
	"testing"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
)

// TestGetPeersForResponse tests the getPeersForResponse function
func TestGetPeersForResponse_LocalPeersOnly(t *testing.T) {
	cfg := &config.Config{AnnounceInterval: 30}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	ra := &ReceiverAnnounce{
		Config:  cfg,
		Storage: storage,
	}

	infoHash := common.InfoHash("12345678901234567890")

	// Add 1 seeder (left=0) and 2 leechers (left>0)
	seedReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", "started", "ua", "", "", nil)
	leech1Req, _ := tracker.MakeRequest("203.0.113.11", string(infoHash), strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	leech2Req, _ := tracker.MakeRequest("203.0.113.12", string(infoHash), strings.Repeat("c", 20), "51415", "0", "0", "200", "", "", "started", "ua", "", "", nil)

	storage.Update(*seedReq)
	storage.Update(*leech1Req)
	storage.Update(*leech2Req)

	seeders, leechers, peers := ra.getPeersForResponse(infoHash)

	if seeders != 1 {
		t.Errorf("expected 1 seeder, got %d", seeders)
	}
	if leechers != 2 {
		t.Errorf("expected 2 leechers, got %d", leechers)
	}
	if len(peers) != 3 {
		t.Errorf("expected 3 peers, got %d", len(peers))
	}
}

func TestGetPeersForResponse_WithForwarderPeers(t *testing.T) {
	cfg := &config.Config{AnnounceInterval: 30}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	ra := &ReceiverAnnounce{
		Config:           cfg,
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
	}

	infoHash := common.InfoHash("12345678901234567890")

	// Add 1 local leecher
	leechReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	storage.Update(*leechReq)

	// Add 2 forwarder peers (counted as seeders)
	forwarderPeers := []common.Peer{
		{IP: common.Address("203.0.113.20"), Port: 6881, PeerID: common.PeerID(strings.Repeat("x", 20))},
		{IP: common.Address("203.0.113.21"), Port: 6882, PeerID: common.PeerID(strings.Repeat("y", 20))},
	}
	forwarderStorage.UpdatePeers(infoHash, "tracker1", forwarderPeers, 1800)

	seeders, leechers, peers := ra.getPeersForResponse(infoHash)

	if seeders != 2 {
		t.Errorf("expected 2 seeders (forwarder peers), got %d", seeders)
	}
	if leechers != 1 {
		t.Errorf("expected 1 leecher (local), got %d", leechers)
	}
	if len(peers) != 3 {
		t.Errorf("expected 3 peers total, got %d", len(peers))
	}
}

func TestGetPeersForResponse_IPDeduplication(t *testing.T) {
	cfg := &config.Config{AnnounceInterval: 30}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	ra := &ReceiverAnnounce{
		Config:           cfg,
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
	}

	infoHash := common.InfoHash("12345678901234567890")

	// Add local peer with IP 203.0.113.10
	localReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", "started", "ua", "", "", nil)
	storage.Update(*localReq)

	// Add forwarder peer with same IP (should not be double-counted)
	forwarderPeers := []common.Peer{
		{IP: common.Address("203.0.113.10"), Port: 6881, PeerID: common.PeerID(strings.Repeat("x", 20))},
		{IP: common.Address("203.0.113.20"), Port: 6882, PeerID: common.PeerID(strings.Repeat("y", 20))},
	}
	forwarderStorage.UpdatePeers(infoHash, "tracker1", forwarderPeers, 1800)

	seeders, leechers, peers := ra.getPeersForResponse(infoHash)

	// 203.0.113.10 should only be counted once (as seeder from local)
	// 203.0.113.20 should be counted once (as seeder from forwarder)
	if seeders != 2 {
		t.Errorf("expected 2 seeders after IP dedup, got %d", seeders)
	}
	if leechers != 0 {
		t.Errorf("expected 0 leechers, got %d", leechers)
	}
	// peers slice includes all entries (no dedup there, that's done in filterPeers)
	if len(peers) != 3 {
		t.Errorf("expected 3 peers in slice, got %d", len(peers))
	}
}

func TestGetPeersForResponse_CompletedEventCountsAsSeeder(t *testing.T) {
	cfg := &config.Config{AnnounceInterval: 30}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	ra := &ReceiverAnnounce{
		Config:  cfg,
		Storage: storage,
	}

	infoHash := common.InfoHash("12345678901234567890")

	// Add peer with completed event (should be seeder even if left > 0)
	completedReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "100", "", "", "completed", "ua", "", "", nil)
	storage.Update(*completedReq)

	seeders, leechers, _ := ra.getPeersForResponse(infoHash)

	if seeders != 1 {
		t.Errorf("expected 1 seeder (completed event), got %d", seeders)
	}
	if leechers != 0 {
		t.Errorf("expected 0 leechers, got %d", leechers)
	}
}

func TestGetPeersForResponse_EmptyStorage(t *testing.T) {
	cfg := &config.Config{AnnounceInterval: 30}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	ra := &ReceiverAnnounce{
		Config:  cfg,
		Storage: storage,
	}

	infoHash := common.InfoHash("12345678901234567890")

	seeders, leechers, peers := ra.getPeersForResponse(infoHash)

	if seeders != 0 {
		t.Errorf("expected 0 seeders, got %d", seeders)
	}
	if leechers != 0 {
		t.Errorf("expected 0 leechers, got %d", leechers)
	}
	if len(peers) != 0 {
		t.Errorf("expected 0 peers, got %d", len(peers))
	}
}

func TestProcessAnnounceRespectsNumwantAndSetsFields(t *testing.T) {
	cfg := &config.Config{
		AnnounceInterval: 30,
		TrackerID:        "tracker-1",
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

	// MinInterval now equals AnnounceInterval in simplified single-interval logic
	if resp.MinInterval != cfg.AnnounceInterval {
		t.Fatalf("min interval mismatch: expected %d, got %d", cfg.AnnounceInterval, resp.MinInterval)
	}
	if resp.Interval != cfg.AnnounceInterval {
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
