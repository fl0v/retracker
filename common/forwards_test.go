package common_test

import (
	"testing"

	"github.com/fl0v/retracker/common"
)

func TestForwardProtocolAndName(t *testing.T) {
	for _, tc := range []struct{ uri, name, protocol, wantName string }{
		{"http://tracker.example/announce", "", "http", "tracker.example"},
		{"https://tracker.example:443/announce", "", "https", "tracker.example:443"},
		{"udp://tracker.example:6969/announce", "", "udp", "tracker.example:6969"},
		{"udp://[2001:db8::1]:6969/announce", "", "udp", "[2001:db8::1]:6969"},
		{"https://tracker.example/announce", "friendly", "https", "friendly"},
		{"ftp://tracker.example/announce", "", "http", "tracker.example"},
		{"http://%", "", "http", "http://%"},
		{"tracker.example/announce", "", "http", "tracker.example/announce"},
		{"", "", "http", ""},
	} {
		t.Run(tc.uri+tc.name, func(t *testing.T) {
			forward := common.Forward{Uri: tc.uri, Name: tc.name}
			if got := forward.GetProtocol(); got != tc.protocol {
				t.Fatalf("protocol=%q, want %q", got, tc.protocol)
			}
			if got := forward.GetName(); got != tc.wantName {
				t.Fatalf("name=%q, want %q", got, tc.wantName)
			}
		})
	}
}
