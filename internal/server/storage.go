// storage.go - Thread-safe in-memory peer storage.
// Structure: map[InfoHash]map[PeerID]Request with mutex protection.
// Includes background purgeRoutine that removes stale peers based on Config.Age.
package server

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
)

var DebugLog = log.New(os.Stdout, `debug#`, log.Lshortfile)

const unknownClient = "unknown"

type Storage struct {
	Config              *config.Config
	Requests            map[common.InfoHash]map[common.PeerID]tracker.Request
	requestsMu          sync.Mutex
	disableStatsRoutine bool // If true, don't start separate stats routine (stats printed by ForwarderManager)
}

func (s *Storage) Update(request tracker.Request) {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	if _, ok := s.Requests[request.InfoHash]; !ok {
		s.Requests[request.InfoHash] = make(map[common.PeerID]tracker.Request)
	}
	s.Requests[request.InfoHash][request.PeerID] = request
}

func (s *Storage) Delete(request tracker.Request) {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	if requests, ok := s.Requests[request.InfoHash]; ok {
		delete(requests, request.PeerID)
		if len(requests) == 0 {
			delete(s.Requests, request.InfoHash)
		}
	}
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
	for {
		time.Sleep(1 * time.Minute)
		if s.Config.Debug {
			DebugLog.Printf("In memory %d hashes\n", len(s.Requests))
			DebugLog.Println(`Locking...`)
		}
		s.requestsMu.Lock()
		for hash, requests := range s.Requests {
			if s.Config.Debug {
				DebugLog.Printf("%d peer in hash %x\n", len(requests), hash)
			}
			for peerId, request := range requests {
				timestampDelta := request.TimeStampDelta()
				if s.Config.Debug {
					DebugLog.Printf(" %x %s:%d %v\n", peerId, request.Peer().IP, request.Peer().Port, timestampDelta)
				}
				if timestampDelta > s.Config.Age {
					DebugLog.Printf("delete peer %x in hash %x\n", peerId, hash)
					delete(s.Requests[hash], peerId)
				}
			}
			if len(requests) == 0 {
				DebugLog.Printf("delete hash %x\n", hash)
				delete(s.Requests, hash)
			}
		}
		s.requestsMu.Unlock()
		if s.Config.Debug {
			DebugLog.Println(`Unlocked`)
		}
	}
}

func (s *Storage) statsRoutine() {
	if s.Config.StatsInterval <= 0 {
		return
	}
	// Check if stats are being handled by ForwarderManager
	if s.disableStatsRoutine {
		return
	}
	ticker := time.NewTicker(time.Duration(s.Config.StatsInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Double-check in case flag was set after routine started
		if s.disableStatsRoutine {
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
		fmt.Printf("Active clients: 0\n")
	} else {
		fmt.Printf("Active clients: %d\n", len(clients))
		for clientKey, info := range clients {
			secondsSinceLastRequest := int(now.Sub(info.lastRequest).Seconds())
			fmt.Printf("  %s: %d announced hash(es), %d seconds since last request\n",
				clientKey, info.hashCount, secondsSinceLastRequest)
		}
	}
}

func NewStorage(cfg *config.Config) *Storage {
	storage := Storage{
		Config:   cfg,
		Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request),
	}
	go storage.purgeRoutine()
	if !storage.disableStatsRoutine {
		go storage.statsRoutine()
	}
	return &storage
}
