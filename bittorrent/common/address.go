package common

import (
	"errors"
	"net"
)

type Address string

func (a *Address) Valid() bool {
	return len(*a) != 0
}

func (a *Address) IPv4() (net.IP, error) {
	if ip := net.ParseIP(a.String()); ip != nil {
		if ip = ip.To4(); ip != nil {
			return ip, nil
		}
	}
	return nil, errors.New("can't convert to IPv4")
}

func (a *Address) IPv6() (net.IP, error) {
	if ip := net.ParseIP(a.String()); ip != nil {
		if ip = ip.To16(); ip != nil {
			return ip, nil
		}
	}
	return nil, errors.New("can't convert to IPv6")
}

func (a *Address) String() string {
	return string(*a)
}
