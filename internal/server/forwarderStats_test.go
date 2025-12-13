package server

import (
	"strings"
	"testing"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
)

func makeTestConfig() *config.Config {
	return &config.Config{
		AnnounceInterval:   30,
		ForwarderQueueSize: 10,
	}
}

func TestGetHashPeerStats_LocalPeersOnly(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	infoHash := common.InfoHash("12345678901234567890")

	// Add 1 seeder and 2 leechers
	seedReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", "started", "ua", "", "", nil)
	leech1Req, _ := tracker.MakeRequest("203.0.113.11", string(infoHash), strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	leech2Req, _ := tracker.MakeRequest("203.0.113.12", string(infoHash), strings.Repeat("c", 20), "51415", "0", "0", "200", "", "", "started", "ua", "", "", nil)

	mainStorage.Update(*seedReq)
	mainStorage.Update(*leech1Req)
	mainStorage.Update(*leech2Req)

	stats := provider.GetHashPeerStats()

	hashKey := "3132333435363738393031323334353637383930" // hex of infoHash
	stat, exists := stats[hashKey]
	if !exists {
		t.Fatalf("expected stats for hash %s", hashKey)
	}

	if stat.Complete != 1 {
		t.Errorf("expected 1 seeder (complete), got %d", stat.Complete)
	}
	if stat.Incomplete != 2 {
		t.Errorf("expected 2 leechers (incomplete), got %d", stat.Incomplete)
	}
	if stat.LocalUnique != 3 {
		t.Errorf("expected 3 local unique peers, got %d", stat.LocalUnique)
	}
	if stat.ForwarderUnique != 0 {
		t.Errorf("expected 0 forwarder peers, got %d", stat.ForwarderUnique)
	}
}

func TestGetHashPeerStats_WithForwarderPeers(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	infoHash := common.InfoHash("12345678901234567890")

	// Add 1 local leecher
	leechReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	mainStorage.Update(*leechReq)

	// Add 2 forwarder peers (counted as seeders)
	forwarderPeers := []common.Peer{
		{IP: common.Address("203.0.113.20"), Port: 6881, PeerID: common.PeerID(strings.Repeat("x", 20))},
		{IP: common.Address("203.0.113.21"), Port: 6882, PeerID: common.PeerID(strings.Repeat("y", 20))},
	}
	forwarderStorage.UpdatePeers(infoHash, "tracker1", forwarderPeers, 1800)

	stats := provider.GetHashPeerStats()

	hashKey := "3132333435363738393031323334353637383930"
	stat, exists := stats[hashKey]
	if !exists {
		t.Fatalf("expected stats for hash %s", hashKey)
	}

	if stat.Complete != 2 {
		t.Errorf("expected 2 seeders (forwarder peers only, local has left>0), got %d", stat.Complete)
	}
	if stat.Incomplete != 1 {
		t.Errorf("expected 1 leecher, got %d", stat.Incomplete)
	}
	if stat.LocalUnique != 1 {
		t.Errorf("expected 1 local peer, got %d", stat.LocalUnique)
	}
	if stat.ForwarderUnique != 2 {
		t.Errorf("expected 2 forwarder peers, got %d", stat.ForwarderUnique)
	}
}

func TestGetHashPeerStats_IPDeduplication(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	infoHash := common.InfoHash("12345678901234567890")

	// Add local seeder with IP 203.0.113.10
	localReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", "started", "ua", "", "", nil)
	mainStorage.Update(*localReq)

	// Add forwarder peer with same IP (should not be double-counted in Complete)
	forwarderPeers := []common.Peer{
		{IP: common.Address("203.0.113.10"), Port: 6881, PeerID: common.PeerID(strings.Repeat("x", 20))},
		{IP: common.Address("203.0.113.20"), Port: 6882, PeerID: common.PeerID(strings.Repeat("y", 20))},
	}
	forwarderStorage.UpdatePeers(infoHash, "tracker1", forwarderPeers, 1800)

	stats := provider.GetHashPeerStats()

	hashKey := "3132333435363738393031323334353637383930"
	stat, exists := stats[hashKey]
	if !exists {
		t.Fatalf("expected stats for hash %s", hashKey)
	}

	// 203.0.113.10 counted once (local seeder), 203.0.113.20 counted once (forwarder seeder)
	if stat.Complete != 2 {
		t.Errorf("expected 2 seeders after IP dedup, got %d", stat.Complete)
	}
	if stat.Incomplete != 0 {
		t.Errorf("expected 0 leechers, got %d", stat.Incomplete)
	}
	// LocalUnique and ForwarderUnique count entries, not unique IPs
	if stat.LocalUnique != 1 {
		t.Errorf("expected 1 local unique, got %d", stat.LocalUnique)
	}
	if stat.ForwarderUnique != 2 {
		t.Errorf("expected 2 forwarder unique, got %d", stat.ForwarderUnique)
	}
}

func TestGetHashPeerStats_MultipleHashes(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	hash1 := common.InfoHash("hash1---------------")
	hash2 := common.InfoHash("hash2---------------")

	// Add peers for hash1
	req1, _ := tracker.MakeRequest("203.0.113.10", string(hash1), strings.Repeat("a", 20), "51413", "0", "0", "0", "", "", "started", "ua", "", "", nil)
	mainStorage.Update(*req1)

	// Add peers for hash2
	req2, _ := tracker.MakeRequest("203.0.113.20", string(hash2), strings.Repeat("b", 20), "51414", "0", "0", "100", "", "", "started", "ua", "", "", nil)
	mainStorage.Update(*req2)

	stats := provider.GetHashPeerStats()

	if len(stats) != 2 {
		t.Errorf("expected 2 hashes in stats, got %d", len(stats))
	}

	hash1Key := "68617368312d2d2d2d2d2d2d2d2d2d2d2d2d2d2d"
	hash2Key := "68617368322d2d2d2d2d2d2d2d2d2d2d2d2d2d2d"

	stat1, exists1 := stats[hash1Key]
	stat2, exists2 := stats[hash2Key]

	if !exists1 {
		t.Fatalf("expected stats for hash1")
	}
	if !exists2 {
		t.Fatalf("expected stats for hash2")
	}

	if stat1.Complete != 1 || stat1.Incomplete != 0 {
		t.Errorf("hash1: expected 1 seeder 0 leechers, got %d/%d", stat1.Complete, stat1.Incomplete)
	}
	if stat2.Complete != 0 || stat2.Incomplete != 1 {
		t.Errorf("hash2: expected 0 seeders 1 leecher, got %d/%d", stat2.Complete, stat2.Incomplete)
	}
}

func TestGetHashPeerStats_CompletedEventCountsAsSeeder(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	infoHash := common.InfoHash("12345678901234567890")

	// Add peer with completed event (should be seeder even if left > 0)
	completedReq, _ := tracker.MakeRequest("203.0.113.10", string(infoHash), strings.Repeat("a", 20), "51413", "0", "0", "100", "", "", "completed", "ua", "", "", nil)
	mainStorage.Update(*completedReq)

	stats := provider.GetHashPeerStats()

	hashKey := "3132333435363738393031323334353637383930"
	stat, exists := stats[hashKey]
	if !exists {
		t.Fatalf("expected stats for hash")
	}

	if stat.Complete != 1 {
		t.Errorf("expected 1 seeder (completed event), got %d", stat.Complete)
	}
	if stat.Incomplete != 0 {
		t.Errorf("expected 0 leechers, got %d", stat.Incomplete)
	}
}

func TestGetHashPeerStats_EmptyStorage(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	stats := provider.GetHashPeerStats()

	if len(stats) != 0 {
		t.Errorf("expected 0 hashes in stats, got %d", len(stats))
	}
}

func TestGetHashPeerStats_ForwarderOnlyHash(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	infoHash := common.InfoHash("12345678901234567890")

	// Add only forwarder peers (no local peers)
	forwarderPeers := []common.Peer{
		{IP: common.Address("203.0.113.20"), Port: 6881, PeerID: common.PeerID(strings.Repeat("x", 20))},
	}
	forwarderStorage.UpdatePeers(infoHash, "tracker1", forwarderPeers, 1800)

	stats := provider.GetHashPeerStats()

	hashKey := "3132333435363738393031323334353637383930"
	stat, exists := stats[hashKey]
	if !exists {
		t.Fatalf("expected stats for hash")
	}

	if stat.Complete != 1 {
		t.Errorf("expected 1 seeder (forwarder peer), got %d", stat.Complete)
	}
	if stat.Incomplete != 0 {
		t.Errorf("expected 0 leechers, got %d", stat.Incomplete)
	}
	if stat.LocalUnique != 0 {
		t.Errorf("expected 0 local peers, got %d", stat.LocalUnique)
	}
	if stat.ForwarderUnique != 1 {
		t.Errorf("expected 1 forwarder peer, got %d", stat.ForwarderUnique)
	}
}

func TestGetHashPeerStats_MultipleForwarders(t *testing.T) {
	cfg := makeTestConfig()
	mainStorage := &Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	forwarderStorage := NewForwarderStorage()

	fm := &ForwarderManager{
		Config:      cfg,
		Storage:     forwarderStorage,
		MainStorage: mainStorage,
	}

	provider := &forwarderStatsProvider{fm: fm, cfg: cfg, now: time.Now()}

	infoHash := common.InfoHash("12345678901234567890")

	// Add peers from multiple forwarders
	peers1 := []common.Peer{
		{IP: common.Address("203.0.113.20"), Port: 6881, PeerID: common.PeerID(strings.Repeat("x", 20))},
	}
	peers2 := []common.Peer{
		{IP: common.Address("203.0.113.21"), Port: 6882, PeerID: common.PeerID(strings.Repeat("y", 20))},
		{IP: common.Address("203.0.113.22"), Port: 6883, PeerID: common.PeerID(strings.Repeat("z", 20))},
	}
	forwarderStorage.UpdatePeers(infoHash, "tracker1", peers1, 1800)
	forwarderStorage.UpdatePeers(infoHash, "tracker2", peers2, 1800)

	stats := provider.GetHashPeerStats()

	hashKey := "3132333435363738393031323334353637383930"
	stat, exists := stats[hashKey]
	if !exists {
		t.Fatalf("expected stats for hash")
	}

	if stat.Complete != 3 {
		t.Errorf("expected 3 seeders from forwarders, got %d", stat.Complete)
	}
	if stat.ForwarderUnique != 3 {
		t.Errorf("expected 3 forwarder peers total, got %d", stat.ForwarderUnique)
	}
}
