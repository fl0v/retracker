package main

type Core struct {
	Config           *Config
	Storage          *Storage
	ForwarderStorage *ForwarderStorage
	ForwarderManager *ForwarderManager
	Receiver         *Receiver
}

func NewCore(config *Config, tempStorage *TempStorage) *Core {
	storage := NewStorage(config)

	var forwarderStorage *ForwarderStorage
	var forwarderManager *ForwarderManager

	// Initialize forwarder system if forwards are configured
	if len(config.Forwards) > 0 {
		forwarderStorage = NewForwarderStorage()

		var prometheus *Prometheus
		// Prometheus will be set later if enabled

		forwarderManager = NewForwarderManager(config, forwarderStorage, prometheus, tempStorage)
		forwarderManager.Start()
	}

	core := Core{
		Config:           config,
		Storage:          storage,
		ForwarderStorage: forwarderStorage,
		ForwarderManager: forwarderManager,
		Receiver:         NewReceiver(config, storage, forwarderStorage, forwarderManager),
	}
	core.Receiver.Announce.TempStorage = tempStorage
	return &core
}
