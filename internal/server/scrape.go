// scrape.go - HTTP and UDP scrape request handler (BEP 48).
// Returns statistics (complete/incomplete/downloaded) for requested info_hashes.
// Aggregates counts from both local storage and forwarder storage.
package server

import (
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/zeebo/bencode"

	"github.com/fl0v/retracker/bittorrent/common"
)

var (
	DebugLogScrape = log.New(os.Stdout, `debug#`, log.Lshortfile)
	ErrorLogScrape = log.New(os.Stderr, `error#`, log.Lshortfile)
)

var (
	ErrNoInfohashes = errors.New(`no infohashes found`)
	ErrBadInfohash  = errors.New(`bad infohash`)
)

// ScrapeStats holds statistics for a single info hash
type ScrapeStats struct {
	Complete   int // Number of seeders
	Incomplete int // Number of leechers
	Downloaded int // Number of completed downloads (not accurately tracked)
}

// ScrapeResponseHash is the HTTP bencoded response format
type ScrapeResponseHash struct {
	Complete   int `bencode:"complete"`
	Incomplete int `bencode:"incomplete"`
	Downloaded int `bencode:"downloaded"`
}

// ScrapeResponse is the HTTP bencoded response format
type ScrapeResponse struct {
	Files map[string]ScrapeResponseHash `bencode:"files"`
}

// getScrapeStats collects statistics for the given info hashes.
// This is the unified logic used by both HTTP and UDP scrape handlers.
func getScrapeStats(storage *Storage, forwarderStorage *ForwarderStorage, infoHashes []common.InfoHash) map[common.InfoHash]ScrapeStats {
	stats := make(map[common.InfoHash]ScrapeStats)

	storage.requestsMu.Lock()
	defer storage.requestsMu.Unlock()

	for _, infoHash := range infoHashes {
		if !infoHash.Valid() {
			continue
		}

		srh := ScrapeStats{}

		// Track unique IPs across local and forwarder peers
		seenIPs := make(map[string]struct{})

		// Local peers from in-memory storage
		if requestInfoHash, found := storage.Requests[infoHash]; found {
			for _, peerRequest := range requestInfoHash {
				ipStr := string(peerRequest.Peer().IP)
				if ipStr == "" {
					continue
				}
				if _, exists := seenIPs[ipStr]; exists {
					continue
				}
				seenIPs[ipStr] = struct{}{}

				if peerRequest.Event == EventCompleted || peerRequest.Left == 0 {
					srh.Complete++
				} else {
					srh.Incomplete++
				}
			}
		}

		// Forwarder peers (unique by IP, counted as seeders because we lack state)
		if forwarderStorage != nil {
			for _, peer := range forwarderStorage.GetAllPeers(infoHash) {
				ipStr := string(peer.IP)
				if ipStr == "" {
					continue
				}
				if _, exists := seenIPs[ipStr]; exists {
					continue
				}
				seenIPs[ipStr] = struct{}{}
				srh.Complete++ // assume forwarder peers are seeders
			}
		}

		// Downloaded count is set to Complete (we don't track actual completed count)
		srh.Downloaded = srh.Complete

		stats[infoHash] = srh
	}

	return stats
}

func (core *Core) getScrapeResponse(infoHashes []string) (ScrapeResponse, error) {
	if len(infoHashes) == 0 {
		ErrorLogScrape.Println(ErrNoInfohashes.Error())
		return ScrapeResponse{}, ErrNoInfohashes
	}

	// Convert string hashes to InfoHash
	infoHashList := make([]common.InfoHash, 0, len(infoHashes))
	for _, infoHashString := range infoHashes {
		infoHash := common.InfoHash(infoHashString)
		if !infoHash.Valid() {
			ErrorLogScrape.Println(infoHashString, ErrBadInfohash.Error())
			return ScrapeResponse{}, ErrBadInfohash
		}
		infoHashList = append(infoHashList, infoHash)
	}

	// Get unified statistics
	stats := getScrapeStats(core.Storage, core.ForwarderStorage, infoHashList)

	// Convert to HTTP response format
	scrapeResponse := ScrapeResponse{
		Files: make(map[string]ScrapeResponseHash),
	}

	for _, infoHashString := range infoHashes {
		infoHash := common.InfoHash(infoHashString)
		infoHashHex := strings.ToUpper(hex.EncodeToString([]byte(infoHashString)))
		if stat, ok := stats[infoHash]; ok {
			DebugLogScrape.Printf("%s\tComplete (seed): %d\tIncomplete (leech): %d", infoHashHex, stat.Complete, stat.Incomplete)
			scrapeResponse.Files[infoHashString] = ScrapeResponseHash(stat)
		} else {
			DebugLogScrape.Printf("%s not found in storage", infoHashHex)
			// Include empty entry for invalid/missing hash
			scrapeResponse.Files[infoHashString] = ScrapeResponseHash{}
		}
	}

	return scrapeResponse, nil
}

func (core *Core) HTTPScrapeHandler(w http.ResponseWriter, r *http.Request) {
	xrealip := r.Header.Get(`X-Real-IP`)
	DebugLogScrape.Printf("%s %s %s '%s' '%s'\n", r.Method, r.RemoteAddr, xrealip, r.RequestURI, r.UserAgent())
	query := r.URL.Query()
	scrapeResponse, err := core.getScrapeResponse(query["info_hash"])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	encoder := bencode.NewEncoder(w)
	if err := encoder.Encode(scrapeResponse); err != nil {
		ErrorLogScrape.Println(err.Error())
	}
}
