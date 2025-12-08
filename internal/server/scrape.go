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

type ScrapeResponseHash struct {
	Complete   int `bencode:"complete"`
	Incomplete int `bencode:"incomplete"`
	Downloaded int `bencode:"downloaded"`
}

type ScrapeResponse struct {
	Files map[string]ScrapeResponseHash `bencode:"files"`
}

func (core *Core) getScrapeResponse(infoHashes []string) (ScrapeResponse, error) {
	if len(infoHashes) == 0 {
		ErrorLogScrape.Println(ErrNoInfohashes.Error())
		return ScrapeResponse{}, ErrNoInfohashes
	}
	scrapeResponse := ScrapeResponse{
		Files: make(map[string]ScrapeResponseHash),
	}

	core.Storage.requestsMu.Lock()
	defer core.Storage.requestsMu.Unlock()
	for _, infoHashString := range infoHashes {
		infoHash := common.InfoHash(infoHashString)
		infoHashHex := strings.ToUpper(hex.EncodeToString([]byte(infoHashString)))
		if !infoHash.Valid() {
			ErrorLogScrape.Println(infoHashString, ErrBadInfohash.Error())
			return ScrapeResponse{}, ErrBadInfohash
		}

		srh := ScrapeResponseHash{}

		// Track unique IPs across local and forwarder peers
		seenIPs := make(map[string]struct{})

		// Local peers from in-memory storage
		if requestInfoHash, found := core.Storage.Requests[infoHash]; found {
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
		} else {
			DebugLogScrape.Printf("%s not found in storage", infoHashHex)
		}

		// Forwarder peers (unique by IP, counted as seeders because we lack state)
		if core.ForwarderStorage != nil {
			for _, peer := range core.ForwarderStorage.GetAllPeers(infoHash) {
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
		DebugLogScrape.Printf("%s\tComplete (seed): %d\tIncomplete (leech): %d", infoHashHex, srh.Complete, srh.Incomplete)
		srh.Downloaded = srh.Complete // Unfortunately, we do not collect statistics to present the actual value.

		scrapeResponse.Files[infoHashString] = srh
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
