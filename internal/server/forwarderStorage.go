package server

import (
	"sync"
	"time"

	"github.com/vvampirius/retracker/bittorrent/common"
	"github.com/vvampirius/retracker/bittorrent/tracker"
	CoreCommon "github.com/vvampirius/retracker/common"
)

type ForwarderPeerEntry struct {
	Peers        []common.Peer
	Interval     int // seconds from tracker response
	LastUpdate   time.Time
	NextAnnounce time.Time
}

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

// GetAverageInterval returns the average interval from all forwarders that have responded
func (fs *ForwarderStorage) GetAverageInterval(infoHash common.InfoHash) int {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	if forwarders, ok := fs.Entries[infoHash]; ok {
		totalInterval := 0
		count := 0
		for _, entry := range forwarders {
			// Only count forwarders that have actually responded (have peers or have been updated)
			if entry.Interval > 0 {
				totalInterval += entry.Interval
				count++
			}
		}
		if count > 0 {
			return totalInterval / count
		}
	}
	return 0 // No forwarders have responded yet
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
