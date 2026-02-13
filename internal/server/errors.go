package server

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
)

type ForwarderErrorType string

const (
	ForwarderErrorRetryable   ForwarderErrorType = "retryable"
	ForwarderErrorPermanent   ForwarderErrorType = "permanent"
	ForwarderErrorRateLimited ForwarderErrorType = "rate_limited"
)

// ForwarderError is a unified typed error used by both HTTP and UDP forwarders.
// Classification is message-driven with transport-level overrides for permanent network failures.
type ForwarderError struct {
	Type    ForwarderErrorType
	Message string
	Cause   error
}

func (e *ForwarderError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "forwarder error"
}

func (e *ForwarderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *ForwarderError) IsPermanent() bool {
	return e != nil && e.Type == ForwarderErrorPermanent
}

func (e *ForwarderError) ShouldSuspend() bool {
	return e != nil && e.Type == ForwarderErrorRateLimited
}

func classifyForwarderError(err error) *ForwarderError {
	if err == nil {
		return nil
	}

	var existing *ForwarderError
	if errors.As(err, &existing) {
		msg := existing.Error()
		if existing.Type == "" {
			existing.Type = classifyForwarderErrorMessage(msg)
		}
		return existing
	}

	msg := strings.TrimSpace(err.Error())
	errorType := classifyForwarderErrorMessage(msg)
	if isPermanentTransportError(err) {
		errorType = ForwarderErrorPermanent
	}

	return &ForwarderError{
		Type:    errorType,
		Message: msg,
		Cause:   err,
	}
}

func classifyForwarderErrorMessage(message string) ForwarderErrorType {
	msg := strings.ToLower(strings.TrimSpace(message))
	if msg == "" {
		return ForwarderErrorRetryable
	}

	if hasTrackerContext(msg) && isRateLimitMessage(msg) {
		return ForwarderErrorRateLimited
	}

	if isPermanentMessage(msg) {
		return ForwarderErrorPermanent
	}

	return ForwarderErrorRetryable
}

func hasTrackerContext(message string) bool {
	trackerMarkers := []string{
		"tracker error",
		"tracker failure",
		"http status",
	}

	for _, marker := range trackerMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func isRateLimitMessage(message string) bool {
	rateLimitPatterns := []string{
		"http status 429",
		"too many requests",
		"rate limit",
		"ratelimit",
		"rate-limited",
		"slow down",
		"slowdown",
		"throttl",
		"retry later",
		"flood",
	}

	for _, pattern := range rateLimitPatterns {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

func isPermanentMessage(message string) bool {
	permanentPatterns := []string{
		"http status 400",
		"http status 403",
		"http status 404",
		"tracker rejection",
		"no such host",
		"unknown host",
		"connection refused",
		"permission denied",
		"tls: unrecognized name",
		"x509:",
		"certificate is not valid for",
		"tls: failed to verify certificate",
		"no route to host",
		"network is unreachable",
	}

	for _, pattern := range permanentPatterns {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

func isPermanentTransportError(err error) bool {
	if err == nil {
		return false
	}

	// DNS/hostname resolution failures are permanent.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// Connection refused means closed port or unreachable service endpoint.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		if errors.Is(opErr.Err, syscall.ECONNREFUSED) {
			return true
		}
		if errors.Is(opErr.Err, syscall.EACCES) || errors.Is(opErr.Err, syscall.EPERM) {
			return true
		}
		if strings.Contains(strings.ToLower(opErr.Err.Error()), "no such host") {
			return true
		}
	}

	return false
}

func isTimeoutErr(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// isTrackerRejection checks if an HTTP status code indicates tracker rejection
// Returns true for: 400 (Bad Request), 403 (Forbidden), 404 (Not Found)
// These typically mean the tracker doesn't recognize or rejects the hash
func isTrackerRejection(statusCode int) bool {
	return statusCode == 400 || statusCode == 403 || statusCode == 404
}
