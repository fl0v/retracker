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

func (self *Storage) Update(request tracker.Request) {
	self.requestsMu.Lock()
	defer self.requestsMu.Unlock()
	if _, ok := self.Requests[request.InfoHash]; !ok {
		self.Requests[request.InfoHash] = make(map[common.PeerID]tracker.Request)
	}
	self.Requests[request.InfoHash][request.PeerID] = request
}

func (self *Storage) Delete(request tracker.Request) {
	self.requestsMu.Lock()
	defer self.requestsMu.Unlock()
	if requests, ok := self.Requests[request.InfoHash]; ok {
		delete(requests, request.PeerID)
		if len(requests) == 0 {
			delete(self.Requests, request.InfoHash)
		}
	}
}

func (self *Storage) GetPeers(infoHash common.InfoHash) []common.Peer {
	self.requestsMu.Lock()
	defer self.requestsMu.Unlock()
	peers := make([]common.Peer, 0)
	if requests, ok := self.Requests[infoHash]; ok {
		for _, request := range requests {
			peers = append(peers, request.Peer())
		}
	}
	return peers
}

func (self *Storage) purgeRoutine() {
	for {
		time.Sleep(1 * time.Minute)
		if self.Config.Debug {
			DebugLog.Printf("In memory %d hashes\n", len(self.Requests))
			DebugLog.Println(`Locking...`)
		}
		self.requestsMu.Lock()
		for hash, requests := range self.Requests {
			if self.Config.Debug {
				DebugLog.Printf("%d peer in hash %x\n", len(requests), hash)
			}
			for peerId, request := range requests {
				timestampDelta := request.TimeStampDelta()
				if self.Config.Debug {
					DebugLog.Printf(" %x %s:%d %v\n", peerId, request.Peer().IP, request.Peer().Port, timestampDelta)
				}
				if timestampDelta > self.Config.Age {
					DebugLog.Printf("delete peer %x in hash %x\n", peerId, hash)
					delete(self.Requests[hash], peerId)
				}
			}
			if len(requests) == 0 {
				DebugLog.Printf("delete hash %x\n", hash)
				delete(self.Requests, hash)
			}
		}
		self.requestsMu.Unlock()
		if self.Config.Debug {
			DebugLog.Println(`Unlocked`)
		}
	}
}

func (self *Storage) statsRoutine() {
	if self.Config.StatsInterval <= 0 {
		return
	}
	// Check if stats are being handled by ForwarderManager
	if self.disableStatsRoutine {
		return
	}
	ticker := time.NewTicker(time.Duration(self.Config.StatsInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Double-check in case flag was set after routine started
		if self.disableStatsRoutine {
			return
		}
		self.printClientStats()
	}
}

func (self *Storage) printClientStats() {
	now := time.Now()
	self.printClientStatsInline(now)
}

func (self *Storage) printClientStatsInline(now time.Time) {
	// Map to track clients: "IP:ClientName" -> {hashCount, lastRequestTime}
	type clientInfo struct {
		hashCount       int
		lastRequest     time.Time
		announcedHashes map[string]bool // Track unique hashes
	}

	clients := make(map[string]*clientInfo)

	self.requestsMu.Lock()
	for hash, requests := range self.Requests {
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
	self.requestsMu.Unlock()

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
