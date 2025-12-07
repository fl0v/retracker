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
		requestInfoHash, found := core.Storage.Requests[infoHash]
		if !found {
			DebugLogScrape.Printf("%s not found in storage", infoHashHex)
			continue
		}
		srh := ScrapeResponseHash{}
		for _, peerRequest := range requestInfoHash {
			if peerRequest.Event == EventCompleted || peerRequest.Left == 0 {
				srh.Complete++
			} else {
				srh.Incomplete++
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
