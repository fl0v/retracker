package server

import (
	"github.com/fl0v/retracker/internal/config"
	"github.com/fl0v/retracker/internal/observability"
)

type Core struct {
	Config           *config.Config
	Storage          *Storage
	ForwarderStorage *ForwarderStorage
	ForwarderManager *ForwarderManager
	Receiver         *Receiver
}

func NewCore(cfg *config.Config, tempStorage *TempStorage) *Core {
	storage := NewStorage(cfg)

	var forwarderStorage *ForwarderStorage
	var forwarderManager *ForwarderManager

	// Initialize forwarder system if forwards are configured
	if len(cfg.Forwards) > 0 {
		forwarderStorage = NewForwarderStorage()

		var prometheus *observability.Prometheus
		// Prometheus will be set later if enabled

		// Disable Storage's separate stats routine since ForwarderManager will handle it
		storage.disableStatsRoutine = true

		forwarderManager = NewForwarderManager(cfg, forwarderStorage, storage, prometheus, tempStorage)
		forwarderManager.Start()
	}

	core := Core{
		Config:           cfg,
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
		ForwarderManager: forwarderManager,
		Receiver:         NewReceiver(cfg, storage, forwarderStorage, forwarderManager),
	}
	core.Receiver.Announce.TempStorage = tempStorage
	core.Receiver.UDP.TempStorage = tempStorage
	return &core
}
