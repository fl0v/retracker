package server

import (
	"errors"
	"net"
	"os"
)

func isTimeoutErr(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}
