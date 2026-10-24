// forwarderHTTP.go - HTTP client ownership and bounded tracker response reads.
package server

import (
	"errors"
	"io"
	"net/http"
)

const (
	maxForwarderHTTPResponseBytes = 1 << 20
	maxForwarderHTTPDrainBytes    = 32 << 10
)

var ErrForwarderResponseTooLarge = errors.New("forwarder response exceeds 1 MiB limit")

type forwarderOptions struct {
	httpClient *http.Client
}

type ForwarderOption func(*forwarderOptions)

// WithHTTPClient injects a caller-owned client before workers start.
// The caller configures it before construction and owns closing idle connections.
func WithHTTPClient(client *http.Client) ForwarderOption {
	return func(options *forwarderOptions) { options.httpClient = client }
}

func newForwarderHTTPClient(maxWorkers int) (*http.Client, func()) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// A custom global transport remains caller-owned.
		return &http.Client{Transport: http.DefaultTransport}, nil
	}
	transport := base.Clone()
	transport.MaxIdleConns = max(transport.MaxIdleConns, maxWorkers)
	transport.MaxIdleConnsPerHost = max(transport.MaxIdleConnsPerHost, 2, maxWorkers)
	return &http.Client{Transport: transport}, transport.CloseIdleConnections
}

func readHTTPAnnounceResponse(response *http.Response) ([]byte, error) {
	if response.ContentLength > maxForwarderHTTPResponseBytes {
		return nil, ErrForwarderResponseTooLarge
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxForwarderHTTPResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxForwarderHTTPResponseBytes {
		return nil, ErrForwarderResponseTooLarge
	}
	return payload, nil
}

func discardHTTPResponseBody(body io.Reader) {
	// Reading to EOF preserves connection reuse for small bodies. Larger bodies
	// are closed by the caller without downloading or buffering the remainder.
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxForwarderHTTPDrainBytes))
}
