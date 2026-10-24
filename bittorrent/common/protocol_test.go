package common_test

import (
	"net"
	"strings"
	"testing"

	"github.com/fl0v/retracker/bittorrent/common"
)

func TestIdentifiersRequireTwentyBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		valid bool
	}{
		{"empty", "", false},
		{"short", strings.Repeat("a", 19), false},
		{"exact", strings.Repeat("a", 20), true},
		{"long", strings.Repeat("a", 21), false},
		{"binary", strings.Repeat("\x00\xff", 10), true},
		{"Unicode byte count", strings.Repeat("界", 7), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash, peerID := common.InfoHash(tc.value), common.PeerID(tc.value)
			if hash.Valid() != tc.valid || peerID.Valid() != tc.valid {
				t.Fatalf("identifier validation failed for %d bytes", len(tc.value))
			}
		})
	}
}

func TestAddressConversion(t *testing.T) {
	for _, tc := range []struct{ value, ipv4, ipv6 string }{
		{"192.0.2.1", "192.0.2.1", "192.0.2.1"},
		{"2001:db8::1", "", "2001:db8::1"},
		{"::ffff:192.0.2.1", "192.0.2.1", "::ffff:192.0.2.1"},
		{"not an IP", "", ""},
		{"", "", ""},
	} {
		t.Run(tc.value, func(t *testing.T) {
			address := common.Address(tc.value)
			for _, conversion := range []struct {
				name string
				call func() (net.IP, error)
				want string
			}{{"IPv4", address.IPv4, tc.ipv4}, {"IPv6", address.IPv6, tc.ipv6}} {
				ip, err := conversion.call()
				if conversion.want == "" {
					if err == nil || ip != nil {
						t.Fatalf("%s accepted %q", conversion.name, tc.value)
					}
				} else if err != nil || !ip.Equal(net.ParseIP(conversion.want)) {
					t.Fatalf("%s returned %v, %v; want %s", conversion.name, ip, err, conversion.want)
				}
			}
		})
	}
}

func TestPeerClientIdentification(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"-qB4610-xxxxxxxxxxxx", "qBittorrent 4.6.10"},
		{"-TR300Z-xxxxxxxxxxxx", "Transmission 3.0.0"},
		{"-ZZabcd-xxxxxxxxxxxx", "ZZ abcd"},
		{"M1234567xxxxxxxxxxxx", "Mainline"},
		{"exbc1234xxxxxxxxxxxx", "BitComet"},
		{"FC123456xxxxxxxxxxxx", "FileCroc"},
		{"FD123456xxxxxxxxxxxx", "Free Download Manager"},
		{"abcdefghxxxxxxxxxxxx", "unknown"},
		{"short", "unknown"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			peerID := common.PeerID(tc.value)
			if got := peerID.DecodeClient(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
