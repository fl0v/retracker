package tracker

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/zeebo/bencode"

	"github.com/fl0v/retracker/bittorrent/common"
)

const (
	// DefaultNumWant is the fallback number of peers a client requests when numwant is omitted.
	DefaultNumWant = 50
	// MaxNumWant caps the number of peers returned to avoid oversized responses.
	MaxNumWant = 200
)

func normalizeNumWant(value uint64) uint64 {
	switch {
	case value == 0:
		return DefaultNumWant
	case value > MaxNumWant:
		return MaxNumWant
	default:
		return value
	}
}

type Request struct {
	timestamp  time.Time
	remoteAddr common.Address
	InfoHash   common.InfoHash `bencode:"info_hash"`
	PeerID     common.PeerID   `bencode:"peer_id"`
	Port       int             `bencode:"port"`
	Uploaded   uint64          `bencode:"uploaded"`
	Downloaded uint64          `bencode:"downloaded"`
	Left       uint64          `bencode:"left"`
	IP         common.Address  `bencode:"ip"`
	NumWant    uint64          `bencode:"numwant"`
	Compact    bool            `bencode:"compact"`
	NoPeerID   bool            `bencode:"no_peer_id"`
	Event      string          `bencode:"event"`
	UserAgent  string          `bencode:"user_agent"`
}

func (req *Request) Peer() common.Peer {
	peer := common.Peer{
		PeerID: req.PeerID,
		IP:     req.IP,
		Port:   req.Port,
	}
	if !peer.IP.Valid() {
		peer.IP = req.remoteAddr
	}
	return peer
}

func (req *Request) String() string {
	return fmt.Sprintf("%s info_hash:%x peer_id:%x port:%d ip:%s numwant:%d event:%s", req.remoteAddr, req.InfoHash, req.PeerID, req.Port, req.IP, req.NumWant, req.Event)
}

func (req *Request) TimeStampDelta() float64 {
	return time.Since(req.timestamp).Minutes()
}

func (req *Request) Timestamp() time.Time {
	return req.timestamp
}

func (req *Request) Bencode() (string, error) {
	return bencode.EncodeString(req)
}

func MakeRequest(remoteAddr, infoHash, peerID, port, uploaded, downloaded, left, ip, numwant,
	event, userAgent string, compactFlag string, noPeerIDFlag string, logger *log.Logger,
) (*Request, error) {
	request := Request{timestamp: time.Now(), remoteAddr: common.Address(remoteAddr), UserAgent: userAgent}

	if v := common.InfoHash(infoHash); v.Valid() {
		request.InfoHash = v
	} else {
		return nil, errors.New(`info_hash is not valid`)
	}

	if v := common.PeerID(peerID); v.Valid() {
		request.PeerID = v
	} else {
		return nil, errors.New(`peer_id is not valid`)
	}

	if d, err := strconv.Atoi(port); err == nil {
		request.Port = d
	} else {
		return nil, fmt.Errorf("can't parse 'port' value: %w", err)
	}

	if d, err := strconv.ParseUint(uploaded, 10, 64); err == nil {
		request.Uploaded = d
	} else {
		return nil, fmt.Errorf("can't parse 'uploaded' value: %w", err)
	}

	if d, err := strconv.ParseUint(downloaded, 10, 64); err == nil {
		request.Downloaded = d
	} else {
		return nil, fmt.Errorf("can't parse 'downloaded' value: %w", err)
	}

	if d, err := strconv.ParseUint(left, 10, 64); err == nil {
		request.Left = d
	} else {
		return nil, fmt.Errorf("can't parse 'left' value: %w", err)
	}

	request.IP = common.Address(ip)

	if numwant != `` {
		if d, err := strconv.ParseUint(numwant, 10, 64); err == nil {
			request.NumWant = d
		}
	}

	request.NumWant = normalizeNumWant(request.NumWant)

	if event := event; event == `` || event == `started` || event == `stopped` || event == `completed` {
		request.Event = event
	} else if logger != nil {
		logger.Printf("WARNING! Got '%s' event in announce.\n", event)
	}

	request.Compact = compactFlag == "1"
	request.NoPeerID = noPeerIDFlag == "1"

	return &request, nil
}
