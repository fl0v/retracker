package server

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fl0v/retracker/bittorrent/common"
	"github.com/fl0v/retracker/bittorrent/tracker"
	"github.com/fl0v/retracker/internal/config"
)

func TestUDPBusyAnnounceReturnsError(t *testing.T) {
	cfg := &config.Config{AnnounceInterval: 1800, RetryPeriod: 120, ForwarderQueueSize: 1, QueueRateLimitThreshold: 80}
	storage := &Storage{Config: cfg, Requests: make(map[common.InfoHash]map[common.PeerID]tracker.Request)}
	fs := NewForwarderStorage()
	fm := NewForwarderManager(cfg, fs, storage, nil, nil)
	defer fm.udpForwarder.Stop()
	fm.rateLimiter = &tokenBucket{lastTick: time.Now()}
	fm.jobQueue <- AnnounceJob{}
	ru := NewReceiverUDP(cfg, storage, fs, fm)
	if err := ru.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer ru.Close()
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if deadlineErr := client.SetReadDeadline(time.Now().Add(3 * time.Second)); deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	req := UDPAnnounceRequest{Action: UDPActionAnnounce, TransactionID: 42, Left: 1, NumWant: 50, Port: 6881}
	copy(req.InfoHash[:], strings.Repeat("h", 20))
	copy(req.PeerID[:], strings.Repeat("p", 20))
	var packet bytes.Buffer
	if encodeErr := binary.Write(&packet, binary.BigEndian, req); encodeErr != nil {
		t.Fatal(encodeErr)
	}
	ru.handleAnnounce(packet.Bytes(), client.LocalAddr().(*net.UDPAddr))
	response := make([]byte, 512)
	n, _, err := client.ReadFromUDP(response)
	if err != nil {
		t.Fatal(err)
	}
	if n < 8 || binary.BigEndian.Uint32(response[:4]) != UDPActionError || binary.BigEndian.Uint32(response[4:8]) != req.TransactionID || !strings.Contains(string(response[8:n]), "retry in 120 seconds") {
		t.Fatalf("unexpected busy response: %x", response[:n])
	}
}

func TestUDPPacketOwnershipAndShutdown(t *testing.T) {
	ru := NewReceiverUDP(&config.Config{}, nil, nil, nil)
	if err := ru.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ru.Close() })
	client, err := net.DialUDP("udp4", nil, ru.conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	const packets = 64
	for tx := uint32(1); tx <= packets; tx++ {
		packet := make([]byte, 16)
		binary.BigEndian.PutUint64(packet, UDPConnectMagic)
		binary.BigEndian.PutUint32(packet[12:], tx)
		if _, err := client.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[uint32]bool, packets)
	for range packets {
		var response [16]byte
		n, err := client.Read(response[:])
		if err != nil {
			t.Fatal(err)
		}
		if n != len(response) || binary.BigEndian.Uint32(response[:]) != UDPActionConnect {
			t.Fatalf("unexpected connect response %x", response[:n])
		}
		tx := binary.BigEndian.Uint32(response[4:])
		if tx == 0 || tx > packets || seen[tx] {
			t.Fatalf("corrupted or duplicate transaction ID: %d", tx)
		}
		seen[tx] = true
	}
	closed := make(chan error, 1)
	go func() { closed <- ru.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("UDP receiver failed to stop")
	}
	if err := ru.Close(); err != nil {
		t.Fatalf("repeated Close failed: %v", err)
	}
	if err := ru.Start("127.0.0.1:0"); err == nil {
		t.Fatal("restarting a closed receiver should fail")
	}
}

func TestUDPConnectionCleanupStops(t *testing.T) {
	ru := NewReceiverUDP(&config.Config{}, nil, nil, nil)
	ru.stopChan = make(chan struct{})
	done := make(chan struct{})
	go func() {
		ru.cleanupConnections()
		close(done)
	}()
	close(ru.stopChan)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection cleanup did not stop")
	}
}
