package server

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
)

func isTimeoutErr(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// isNonRetryableError checks if an error indicates a condition that won't improve with retries:
// - Invalid hostname (DNS errors)
// - Port not open (connection refused)
// - Tracker rejection (HTTP 400/403/404)
func isNonRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// Check for DNS/hostname errors
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true // DNS errors are non-retryable
	}

	// Check for connection refused (port not open)
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Err != nil {
			if errors.Is(opErr.Err, syscall.ECONNREFUSED) {
				return true // Connection refused - port not open
			}
			// Check for "no such host" errors
			if strings.Contains(opErr.Err.Error(), "no such host") {
				return true
			}
		}
	}

	// Check error message for common non-retryable patterns
	nonRetryablePatterns := []string{
		"no such host",
		"unknown host",
		"connection refused",
		"no route to host",
		"network is unreachable",
	}

	for _, pattern := range nonRetryablePatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// isTrackerRejection checks if an HTTP status code indicates tracker rejection
// Returns true for: 400 (Bad Request), 403 (Forbidden), 404 (Not Found)
// These typically mean the tracker doesn't recognize or rejects the hash
func isTrackerRejection(statusCode int) bool {
	return statusCode == 400 || statusCode == 403 || statusCode == 404
}
