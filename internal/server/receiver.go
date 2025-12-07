package server

import (
	"github.com/fl0v/retracker/internal/config"
)

type Receiver struct {
	Announce *ReceiverAnnounce
	UDP      *ReceiverUDP
}

func NewReceiver(cfg *config.Config, storage *Storage, forwarderStorage *ForwarderStorage, forwarderManager *ForwarderManager) *Receiver {
	receiver := Receiver{
		Announce: NewReceiverAnnounce(cfg, storage, forwarderStorage, forwarderManager),
		UDP:      NewReceiverUDP(cfg, storage),
	}
	return &receiver
}
