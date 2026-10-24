// http_body_test.go - Bounded JSON input rejects malformed requests atomically.
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddForwarderRejectsInvalidBodiesBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"malformed", `{"urls":["http://tracker.example/announce"],`, 400},
		{"extra JSON", `{"urls":["http://tracker.example/announce"]} {}`, 400},
		{"invalid trailing bytes", `{"urls":[]} trailing`, 400},
		{"oversize", `{"urls":["http://tracker.example/` + strings.Repeat("x", maxAddForwarderBodyBytes) + `"]}`, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := NewCore(makeCfg(), nil, discardLogger())
			defer core.Close()
			w := httptest.NewRecorder()
			core.HTTPAddForwarderHandler(w, httptest.NewRequest(http.MethodPost, "/add-forwarder", strings.NewReader(tc.body)))
			if w.Code != tc.code {
				t.Fatalf("got %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			if core.ForwarderManager != nil {
				t.Fatal("invalid body mutated forwarder state")
			}
		})
	}
}

func TestQueryOnlyForwarderPOSTStillWorks(t *testing.T) {
	core := NewCore(makeCfg(), nil, discardLogger())
	defer core.Close()
	w := httptest.NewRecorder()
	core.HTTPAddForwarderHandler(w, httptest.NewRequest(http.MethodPost, "/add-forwarder?urls=udp://tracker.example:6969/announce", nil))
	if w.Code != 200 || core.ForwarderManager == nil {
		t.Fatalf("query-only POST failed: %d %s", w.Code, w.Body.String())
	}
}
