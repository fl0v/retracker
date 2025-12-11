package server

import (
	"strings"
	"testing"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
)

func TestGetScrapeStatsEmptyHashes(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	stats := getScrapeStats(storage, nil, []common.InfoHash{})
	if len(stats) != 0 {
		t.Fatalf("expected empty stats for empty hashes, got %d entries", len(stats))
	}
}

func TestGetScrapeStatsInvalidHash(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	invalidHash := common.InfoHash("short")
	stats := getScrapeStats(storage, nil, []common.InfoHash{invalidHash})
	if len(stats) != 0 {
		t.Fatalf("expected no stats for invalid hash, got %d entries", len(stats))
	}
}

func TestGetScrapeStatsHashNotFound(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	infoHash := common.InfoHash("12345678901234567890")
	stats := getScrapeStats(storage, nil, []common.InfoHash{infoHash})

	if len(stats) != 1 {
		t.Fatalf("expected 1 entry for hash, got %d", len(stats))
	}

	stat, ok := stats[infoHash]
	if !ok {
		t.Fatalf("expected stats for hash")
	}

	if stat.Complete != 0 || stat.Incomplete != 0 || stat.Downloaded != 0 {
		t.Fatalf("expected all zeros for missing hash, got Complete=%d Incomplete=%d Downloaded=%d",
			stat.Complete, stat.Incomplete, stat.Downloaded)
	}
}

func TestGetScrapeStatsLocalPeersOnly(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	infoHash := common.InfoHash("12345678901234567890")

	// Add a seeder (completed event)
	seedReq, err := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", EventCompleted, "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create seed request: %v", err)
	}
	storage.Update(*seedReq)

	// Add a leecher (left > 0)
	leechReq, err := tracker.MakeRequest("203.0.113.11", string(infoHash), strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create leech request: %v", err)
	}
	storage.Update(*leechReq)

	// Add another seeder (left == 0, no event)
	seedReq2, err := tracker.MakeRequest("203.0.113.12", string(infoHash), strings.Repeat("c", 20), "51415", "0", "0", "0", "", "", "", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create seed request 2: %v", err)
	}
	storage.Update(*seedReq2)

	stats := getScrapeStats(storage, nil, []common.InfoHash{infoHash})

	if len(stats) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(stats))
	}

	stat := stats[infoHash]
	if stat.Complete != 2 {
		t.Fatalf("expected 2 seeders, got %d", stat.Complete)
	}
	if stat.Incomplete != 1 {
		t.Fatalf("expected 1 leecher, got %d", stat.Incomplete)
	}
	if stat.Downloaded != stat.Complete {
		t.Fatalf("expected Downloaded to equal Complete (%d), got %d", stat.Complete, stat.Downloaded)
	}
}

func TestGetScrapeStatsForwarderPeersOnly(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	infoHash := common.InfoHash("12345678901234567890")

	// Add forwarder peers
	peer1 := common.Peer{
		IP:   common.Address("203.0.113.20"),
		Port: 51413,
	}
	peer2 := common.Peer{
		IP:   common.Address("203.0.113.21"),
		Port: 51414,
	}
	forwarderStorage.UpdatePeers(infoHash, "forwarder1", []common.Peer{peer1, peer2}, 30)

	stats := getScrapeStats(storage, forwarderStorage, []common.InfoHash{infoHash})

	if len(stats) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(stats))
	}

	stat := stats[infoHash]
	if stat.Complete != 2 {
		t.Fatalf("expected 2 forwarder peers counted as seeders, got %d", stat.Complete)
	}
	if stat.Incomplete != 0 {
		t.Fatalf("expected 0 leechers, got %d", stat.Incomplete)
	}
}

func TestGetScrapeStatsLocalAndForwarderPeers(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	infoHash := common.InfoHash("12345678901234567890")

	// Add local seeder
	seedReq, err := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", EventCompleted, "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create seed request: %v", err)
	}
	storage.Update(*seedReq)

	// Add local leecher
	leechReq, err := tracker.MakeRequest("203.0.113.11", string(infoHash), strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create leech request: %v", err)
	}
	storage.Update(*leechReq)

	// Add forwarder peer (different IP)
	peer1 := common.Peer{
		IP:   common.Address("203.0.113.20"),
		Port: 51413,
	}
	forwarderStorage.UpdatePeers(infoHash, "forwarder1", []common.Peer{peer1}, 30)

	stats := getScrapeStats(storage, forwarderStorage, []common.InfoHash{infoHash})

	if len(stats) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(stats))
	}

	stat := stats[infoHash]
	if stat.Complete != 2 { // 1 local seeder + 1 forwarder peer
		t.Fatalf("expected 2 seeders (1 local + 1 forwarder), got %d", stat.Complete)
	}
	if stat.Incomplete != 1 {
		t.Fatalf("expected 1 leecher, got %d", stat.Incomplete)
	}
}

func TestGetScrapeStatsIPDeduplication(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	infoHash := common.InfoHash("12345678901234567890")

	// Add local peer with IP 203.0.113.10
	seedReq, err := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", EventCompleted, "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create seed request: %v", err)
	}
	storage.Update(*seedReq)

	// Add forwarder peer with same IP (should be deduplicated)
	peer1 := common.Peer{
		IP:   common.Address("203.0.113.10"), // Same IP as local peer
		Port: 51413,
	}
	forwarderStorage.UpdatePeers(infoHash, "forwarder1", []common.Peer{peer1}, 30)

	stats := getScrapeStats(storage, forwarderStorage, []common.InfoHash{infoHash})

	stat := stats[infoHash]
	// Should only count once (local peer takes precedence)
	if stat.Complete != 1 {
		t.Fatalf("expected 1 seeder after IP deduplication, got %d", stat.Complete)
	}
}

func TestGetScrapeStatsMultipleHashes(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	infoHash1 := common.InfoHash("12345678901234567890")
	infoHash2 := common.InfoHash("abcdefghijklmnopqrst")

	// Add peers for hash1
	seedReq1, err := tracker.MakeRequest("203.0.113.10", string(infoHash1), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", EventCompleted, "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create seed request: %v", err)
	}
	storage.Update(*seedReq1)

	// Add peers for hash2
	leechReq2, err := tracker.MakeRequest("203.0.113.11", string(infoHash2), strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create leech request: %v", err)
	}
	storage.Update(*leechReq2)

	stats := getScrapeStats(storage, nil, []common.InfoHash{infoHash1, infoHash2})

	if len(stats) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(stats))
	}

	stat1 := stats[infoHash1]
	if stat1.Complete != 1 || stat1.Incomplete != 0 {
		t.Fatalf("hash1: expected Complete=1 Incomplete=0, got Complete=%d Incomplete=%d", stat1.Complete, stat1.Incomplete)
	}

	stat2 := stats[infoHash2]
	if stat2.Complete != 0 || stat2.Incomplete != 1 {
		t.Fatalf("hash2: expected Complete=0 Incomplete=1, got Complete=%d Incomplete=%d", stat2.Complete, stat2.Incomplete)
	}
}

func TestGetScrapeResponseEmptyHashes(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	core := &Core{
		Storage: storage,
	}

	_, err := core.getScrapeResponse([]string{})
	if err == nil {
		t.Fatalf("expected error for empty hashes")
	}
}

func TestGetScrapeResponseInvalidHash(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	core := &Core{
		Storage: storage,
	}

	_, err := core.getScrapeResponse([]string{"invalid"})
	if err == nil {
		t.Fatalf("expected error for invalid hash")
	}
}

func TestGetScrapeResponseValidHash(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	infoHash := "12345678901234567890"

	// Add a seeder and leecher
	seedReq, err := tracker.MakeRequest("203.0.113.10", infoHash, strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", EventCompleted, "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create seed request: %v", err)
	}
	storage.Update(*seedReq)

	leechReq, err := tracker.MakeRequest("203.0.113.11", infoHash, strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create leech request: %v", err)
	}
	storage.Update(*leechReq)

	core := &Core{
		Storage: storage,
	}

	response, err := core.getScrapeResponse([]string{infoHash})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(response.Files) != 1 {
		t.Fatalf("expected 1 file in response, got %d", len(response.Files))
	}

	file, ok := response.Files[infoHash]
	if !ok {
		t.Fatalf("expected file entry for hash")
	}

	if file.Complete != 1 {
		t.Fatalf("expected Complete=1, got %d", file.Complete)
	}
	if file.Incomplete != 1 {
		t.Fatalf("expected Incomplete=1, got %d", file.Incomplete)
	}
	if file.Downloaded != file.Complete {
		t.Fatalf("expected Downloaded to equal Complete (%d), got %d", file.Complete, file.Downloaded)
	}
}

func TestGetScrapeResponseMultipleHashes(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}

	infoHash1 := "12345678901234567890"
	infoHash2 := "abcdefghijklmnopqrst"

	// Add peer for hash1
	seedReq1, err := tracker.MakeRequest("203.0.113.10", infoHash1, strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", EventCompleted, "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create seed request: %v", err)
	}
	storage.Update(*seedReq1)

	core := &Core{
		Storage: storage,
	}

	response, err := core.getScrapeResponse([]string{infoHash1, infoHash2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(response.Files) != 2 {
		t.Fatalf("expected 2 files in response, got %d", len(response.Files))
	}

	file1, ok := response.Files[infoHash1]
	if !ok {
		t.Fatalf("expected file entry for hash1")
	}
	if file1.Complete != 1 {
		t.Fatalf("hash1: expected Complete=1, got %d", file1.Complete)
	}

	file2, ok := response.Files[infoHash2]
	if !ok {
		t.Fatalf("expected file entry for hash2")
	}
	if file2.Complete != 0 || file2.Incomplete != 0 {
		t.Fatalf("hash2: expected all zeros, got Complete=%d Incomplete=%d", file2.Complete, file2.Incomplete)
	}
}

func TestGetScrapeResponseWithForwarderPeers(t *testing.T) {
	cfg := &config.Config{}
	storage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	infoHash := "12345678901234567890"

	// Add local leecher
	leechReq, err := tracker.MakeRequest("203.0.113.10", infoHash, strings.Repeat("a", 20), "51413", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	if err != nil {
		t.Fatalf("failed to create leech request: %v", err)
	}
	storage.Update(*leechReq)

	// Add forwarder peer
	peer1 := common.Peer{
		IP:   common.Address("203.0.113.20"),
		Port: 51413,
	}
	forwarderStorage.UpdatePeers(common.InfoHash(infoHash), "forwarder1", []common.Peer{peer1}, 30)

	core := &Core{
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
	}

	response, err := core.getScrapeResponse([]string{infoHash})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	file := response.Files[infoHash]
	if file.Complete != 1 { // forwarder peer counted as seeder
		t.Fatalf("expected Complete=1 (forwarder peer), got %d", file.Complete)
	}
	if file.Incomplete != 1 { // local leecher
		t.Fatalf("expected Incomplete=1 (local leecher), got %d", file.Incomplete)
	}
}
