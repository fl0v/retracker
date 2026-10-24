package response

import (
	"bytes"
	"encoding/binary"
	"net"

	"github.com/zeebo/bencode"

	"github.com/fl0v/retracker/bittorrent/common"
)

type ResponseCompacted struct {
	Interval      int         `bencode:"interval"`
	MinInterval   int         `bencode:"min interval,omitempty"`
	Complete      int         `bencode:"complete,omitempty"`
	Incomplete    int         `bencode:"incomplete,omitempty"`
	TrackerID     string      `bencode:"tracker id,omitempty"`
	FailureReason string      `bencode:"failure reason,omitempty"`
	RetryIn       interface{} `bencode:"retry in,omitempty"` // BEP 31: int (minutes) or "never"
	Peers4        []byte      `bencode:"peers"`
	Peers6        []byte      `bencode:"peers6"`
}

func (rc *ResponseCompacted) Bencode() (string, error) {
	return bencode.EncodeString(rc)
}

func (rc *ResponseCompacted) Response() Response {
	response := Response{
		Interval:      rc.Interval,
		MinInterval:   rc.MinInterval,
		Complete:      rc.Complete,
		Incomplete:    rc.Incomplete,
		TrackerID:     rc.TrackerID,
		FailureReason: rc.FailureReason,
		RetryIn:       rc.RetryIn,
		Peers:         rc.Peers(),
	}
	return response
}

func (rc *ResponseCompacted) Peers() []common.Peer {
	peers := make([]common.Peer, 0, len(rc.Peers4)/6+len(rc.Peers6)/18)
	peers = append(peers, rc.peers4Parse()...)
	peers = append(peers, rc.peers6Parse()...)
	return peers
}

func (rc *ResponseCompacted) peers4Parse() []common.Peer {
	peers := make([]common.Peer, 0)
	buf := bytes.NewBuffer(rc.Peers4)
	for {
		ipBytes := make([]byte, 4)
		if n, err := buf.Read(ipBytes); err != nil || n != 4 {
			break
		}
		portBytes := make([]byte, 2)
		if n, err := buf.Read(portBytes); err != nil || n != 2 {
			break
		}
		peer := common.Peer{
			IP:   common.Address(net.IP(ipBytes).String()),
			Port: int(binary.BigEndian.Uint16(portBytes)),
		}
		peers = append(peers, peer)
	}
	return peers
}

func (rc *ResponseCompacted) peers6Parse() []common.Peer {
	peers := make([]common.Peer, 0)
	buf := bytes.NewBuffer(rc.Peers6)
	for {
		ipBytes := make([]byte, 16)
		if n, err := buf.Read(ipBytes); err != nil || n != 16 {
			break
		}
		portBytes := make([]byte, 2)
		if n, err := buf.Read(portBytes); err != nil || n != 2 {
			break
		}
		peer := common.Peer{
			IP:   common.Address(net.IP(ipBytes).String()),
			Port: int(binary.BigEndian.Uint16(portBytes)),
		}
		peers = append(peers, peer)
	}
	return peers
}

func (rc *ResponseCompacted) ReloadPeers(peers []common.Peer) {
	var peers4 bytes.Buffer
	var peers6 bytes.Buffer
	for _, peer := range peers {
		if ip, err := peer.IP.IPv4(); err == nil {
			peers4.Write([]byte(ip.To4()))
			portBytes := make([]byte, 2)
			binary.BigEndian.PutUint16(portBytes, uint16(peer.Port))
			peers4.Write(portBytes)
			continue
		}
		if ip, err := peer.IP.IPv6(); err == nil {
			peers6.Write([]byte(ip.To16()))
			portBytes := make([]byte, 2)
			binary.BigEndian.PutUint16(portBytes, uint16(peer.Port))
			peers6.Write(portBytes)
			continue
		}
		ErrorLog.Printf("Can't identify IP: %v", peer.IP)
	}
	rc.Peers4 = peers4.Bytes()
	if peers6.Len() > 0 {
		rc.Peers6 = peers6.Bytes()
	} else {
		rc.Peers6 = nil
	}
}
