// storage.go - Thread-safe in-memory peer storage.
// Structure: map[InfoHash]map[PeerID]Request with mutex protection.
// Includes background purgeRoutine that removes stale peers based on Config.Age.
// Peer sessions cancel forwarding jobs on removal or expiry.
// Close cancels peer sessions and joins the purge and stats routines.
// Last-peer deletion clears the paired upstream cache; purge logging runs outside its mutex.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

const unknownClient = "unknown"

type Storage struct {
	observability.Logging
	Config              *config.Config
	Requests            map[common.InfoHash]map[common.PeerID]tracker.Request
	requestsMu          sync.Mutex
	peerSessions        map[peerKey]peerSession
	forwarderStorage    *ForwarderStorage // Protected by requestsMu; shares hash lifetime.
	tasks               *backgroundTasks
	disableStatsRoutine atomic.Bool // Stats are printed by ForwarderManager when true.
}

type peerKey struct {
	infoHash common.InfoHash
	peerID   common.PeerID
}

type peerSession struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *Storage) Update(request tracker.Request) context.Context {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	if _, ok := s.Requests[request.InfoHash]; !ok {
		s.Requests[request.InfoHash] = make(map[common.PeerID]tracker.Request)
	}
	s.Requests[request.InfoHash][request.PeerID] = request
	if s.peerSessions == nil {
		s.peerSessions = make(map[peerKey]peerSession)
	}
	key := peerKey{request.InfoHash, request.PeerID}
	if _, ok := s.peerSessions[key]; !ok {
		parent := context.Background()
		if s.tasks != nil {
			parent = s.tasks.ctx
		}
		ctx, cancel := context.WithCancel(parent)
		s.peerSessions[key] = peerSession{ctx: ctx, cancel: cancel}
	}
	return s.peerSessions[key].ctx
}

func (s *Storage) Delete(request tracker.Request) {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	s.deleteLocked(request.InfoHash, request.PeerID)
}

// deleteLocked ends a peer session so queued and in-flight jobs become stale.
func (s *Storage) deleteLocked(infoHash common.InfoHash, peerID common.PeerID) {
	key := peerKey{infoHash, peerID}
	if session, ok := s.peerSessions[key]; ok {
		session.cancel()
		delete(s.peerSessions, key)
	}
	if requests, ok := s.Requests[infoHash]; ok {
		delete(requests, peerID)
		if len(requests) == 0 {
			delete(s.Requests, infoHash)
			if s.forwarderStorage != nil {
				s.forwarderStorage.Cleanup(infoHash)
			}
		}
	}
}

func (s *Storage) peerContext(infoHash common.InfoHash, peerID common.PeerID) context.Context {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	return s.peerSessions[peerKey{infoHash, peerID}].ctx
}

func (s *Storage) GetPeers(infoHash common.InfoHash) []common.Peer {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	peers := make([]common.Peer, 0)
	if requests, ok := s.Requests[infoHash]; ok {
		for _, request := range requests {
			peers = append(peers, request.Peer())
		}
	}
	return peers
}

func (s *Storage) purgeRoutine() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.tasks.ctx.Done():
			return
		case <-ticker.C:
		}
		s.purgeExpired()

	}
}

// purgeExpired cancels sessions and clears unused upstream caches under one lock.
// Log output happens afterwards so a slow sink cannot hold up peer operations.
func (s *Storage) purgeExpired() {
	peers, hashes := 0, 0
	s.requestsMu.Lock()
	for hash, requests := range s.Requests {
		for peerID, request := range requests {
			if request.TimeStampDelta() > s.Config.Age {
				s.deleteLocked(hash, peerID)
				peers++
			}
		}
		if len(requests) == 0 {
			hashes++
		}
	}
	remaining := len(s.Requests)
	s.requestsMu.Unlock()
	s.Log().Debug("Purged expired peers", "peers", peers, "hashes", hashes, "remaining_hashes", remaining)
}

func (s *Storage) statsRoutine() {
	if s.Config.StatsInterval <= 0 {
		return
	}
	// Check if stats are being handled by ForwarderManager
	if s.disableStatsRoutine.Load() {
		return
	}
	ticker := time.NewTicker(time.Duration(s.Config.StatsInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.tasks.ctx.Done():
			return
		case <-ticker.C:
		}
		// Double-check in case flag was set after routine started
		if s.disableStatsRoutine.Load() {
			return
		}
		s.printClientStats()
	}
}

func (s *Storage) printClientStats() {
	now := time.Now()
	s.printClientStatsInline(now)
}

func (s *Storage) printClientStatsInline(now time.Time) {
	// Map to track clients: "IP:ClientName" -> {hashCount, lastRequestTime}
	type clientInfo struct {
		hashCount       int
		lastRequest     time.Time
		announcedHashes map[string]bool // Track unique hashes
	}

	clients := make(map[string]*clientInfo)

	s.requestsMu.Lock()
	for hash, requests := range s.Requests {
		hashStr := fmt.Sprintf("%x", hash)
		for _, request := range requests {
			// Use IP from request (Peer() already handles fallback to remoteAddr)
			peer := request.Peer()
			clientIP := string(peer.IP)
			if clientIP == "" {
				clientIP = unknownClient
			}

			// Decode client from peer_id (more reliable than User-Agent)
			clientName := request.PeerID.DecodeClient()
			if clientName == unknownClient {
				// Fallback to User-Agent if peer_id decoding fails
				userAgent := request.UserAgent
				if userAgent != "" {
					clientName = userAgent
				} else {
					clientName = unknownClient
				}
			}

			clientKey := fmt.Sprintf("%s:%s", clientIP, clientName)
			if _, ok := clients[clientKey]; !ok {
				clients[clientKey] = &clientInfo{
					hashCount:       0,
					lastRequest:     request.Timestamp(),
					announcedHashes: make(map[string]bool),
				}
			}

			client := clients[clientKey]
			client.announcedHashes[hashStr] = true
			client.hashCount = len(client.announcedHashes)

			// Update last request time if this request is more recent
			requestTime := request.Timestamp()
			if requestTime.After(client.lastRequest) {
				client.lastRequest = requestTime
			}
		}
	}
	s.requestsMu.Unlock()

	// Print client statistics inline (without separate header/footer)
	if len(clients) == 0 {
		s.Log().Info("Active clients", "count", 0)
	} else {
		s.Log().Info("Active clients", "count", len(clients))
		for clientKey, info := range clients {
			secondsSinceLastRequest := int(now.Sub(info.lastRequest).Seconds())
			s.Log().Info("Client statistics", "client", clientKey, "hashes", info.hashCount, "seconds_since_last_request", secondsSinceLastRequest)
		}
	}
}

func NewStorage(cfg *config.Config, loggers ...*slog.Logger) *Storage {
	storage := Storage{
		Logging:  observability.NewLogging(cfg.Debug, loggers...),
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
		tasks:    newBackgroundTasks(),
	}
	storage.tasks.run(storage.purgeRoutine)
	if !storage.disableStatsRoutine.Load() {
		storage.tasks.run(storage.statsRoutine)
	}
	return &storage
}

func (s *Storage) Close() error {
	if s.tasks != nil {
		s.tasks.close()
	}
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	for key, session := range s.peerSessions {
		session.cancel()
		delete(s.peerSessions, key)
	}
	return nil
}
