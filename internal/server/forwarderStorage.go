package server

import (
	"sync"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	CoreCommon "github.com/fl0v/retracker/common"
)

// ForwarderPeerEntry stores the last response from a forwarder for a specific hash.
// Interval is the last reported interval from the tracker for this hash+forwarder combination.
type ForwarderPeerEntry struct {
	Peers        []common.Peer
	Interval     int // Last reported interval in seconds from tracker response (per hash+forwarder)
	LastUpdate   time.Time
	NextAnnounce time.Time // Calculated as LastUpdate + Interval
}

// ForwarderStorage stores forwarder responses and intervals.
//
// Interval Tracking Structure:
// - Intervals are tracked PER-HASH, PER-FORWARDER
// - Structure: map[InfoHash]map[ForwarderName]ForwarderPeerEntry
// - Each hash maintains separate intervals for each forwarder
// - When a forwarder responds for a hash, only that hash's interval for that forwarder is updated
// - Different hashes can have different intervals for the same forwarder
//
// Example:
//
//	Hash1 -> ForwarderA: interval=30s, ForwarderB: interval=60s
//	Hash2 -> ForwarderA: interval=45s, ForwarderB: interval=60s
//	Hash3 -> ForwarderA: interval=30s, ForwarderB: interval=90s
//
// This allows each hash to respect each forwarder's specific interval requirements.
type ForwarderStorage struct {
	// map[InfoHash]map[ForwarderName]ForwarderPeerEntry
	Entries map[common.InfoHash]map[string]ForwarderPeerEntry
	mu      sync.RWMutex
}

type AnnounceJob struct {
	InfoHash      common.InfoHash
	ForwarderName string
	PeerID        common.PeerID
	Forwarder     CoreCommon.Forward
	Request       tracker.Request // template request
	ScheduledTime time.Time       // If set, job should execute at this time (zero means immediate)
}

func NewForwarderStorage() *ForwarderStorage {
	return &ForwarderStorage{
		Entries: make(map[common.InfoHash]map[string]ForwarderPeerEntry),
	}
}

func (fs *ForwarderStorage) UpdatePeers(infoHash common.InfoHash, forwarderName string, peers []common.Peer, interval int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.Entries[infoHash] == nil {
		fs.Entries[infoHash] = make(map[string]ForwarderPeerEntry)
	}

	now := time.Now()
	entry := ForwarderPeerEntry{
		Peers:        peers,
		Interval:     interval,
		LastUpdate:   now,
		NextAnnounce: now.Add(time.Duration(interval) * time.Second),
	}

	fs.Entries[infoHash][forwarderName] = entry
}

func (fs *ForwarderStorage) GetAllPeers(infoHash common.InfoHash) []common.Peer {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	allPeers := make([]common.Peer, 0)
	if forwarders, ok := fs.Entries[infoHash]; ok {
		for _, entry := range forwarders {
			allPeers = append(allPeers, entry.Peers...)
		}
	}
	return allPeers
}

func (fs *ForwarderStorage) GetNextAnnounceJobs(now time.Time) []AnnounceJob {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	jobs := make([]AnnounceJob, 0)
	for infoHash, forwarders := range fs.Entries {
		for forwarderName, entry := range forwarders {
			// Check if it's time to re-announce (NextAnnounce is in the past or now)
			if now.After(entry.NextAnnounce) || now.Equal(entry.NextAnnounce) {
				jobs = append(jobs, AnnounceJob{
					InfoHash:      infoHash,
					ForwarderName: forwarderName,
				})
			}
		}
	}
	return jobs
}

func (fs *ForwarderStorage) MarkAnnounced(infoHash common.InfoHash, forwarderName string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if forwarders, ok := fs.Entries[infoHash]; ok {
		if entry, ok := forwarders[forwarderName]; ok {
			entry.NextAnnounce = time.Now().Add(time.Duration(entry.Interval) * time.Second)
			forwarders[forwarderName] = entry
		}
	}
}

// ShouldAnnounceNow returns true when the tracker has never been contacted for the hash
// or when the stored NextAnnounce is due.
func (fs *ForwarderStorage) ShouldAnnounceNow(infoHash common.InfoHash, forwarderName string, now time.Time) bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	forwarders, ok := fs.Entries[infoHash]
	if !ok {
		return true
	}
	entry, ok := forwarders[forwarderName]
	if !ok {
		return true
	}
	if entry.NextAnnounce.IsZero() {
		return true
	}
	return !entry.NextAnnounce.After(now)
}

func (fs *ForwarderStorage) HasInfoHash(infoHash common.InfoHash) bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	_, ok := fs.Entries[infoHash]
	return ok
}

func (fs *ForwarderStorage) Cleanup(infoHash common.InfoHash) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.Entries, infoHash)
}
