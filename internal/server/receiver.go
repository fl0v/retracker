package server

import (
	"log/slog"

	"github.com/fl0v/retracker/internal/config"
)

type Receiver struct {
	Announce *ReceiverAnnounce
	UDP      *ReceiverUDP
}

func NewReceiver(cfg *config.Config, storage *Storage, forwarderStorage *ForwarderStorage, forwarderManager *ForwarderManager, loggers ...*slog.Logger) *Receiver {
	receiver := Receiver{
		Announce: NewReceiverAnnounce(cfg, storage, forwarderStorage, forwarderManager, loggers...),
		UDP:      NewReceiverUDP(cfg, storage, forwarderStorage, forwarderManager, loggers...),
	}
	return &receiver
}
