package main

type Receiver struct {
	Announce *ReceiverAnnounce
	UDP      *ReceiverUDP
}

func NewReceiver(config *Config, storage *Storage, forwarderStorage *ForwarderStorage, forwarderManager *ForwarderManager) *Receiver {
	receiver := Receiver{
		Announce: NewReceiverAnnounce(config, storage, forwarderStorage, forwarderManager),
		UDP:      NewReceiverUDP(config, storage),
	}
	return &receiver
}
