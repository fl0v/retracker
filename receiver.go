package main

type Receiver struct {
	Announce *ReceiverAnnounce
	UDP      *ReceiverUDP
}

func NewReceiver(config *Config, storage *Storage) *Receiver {
	receiver := Receiver{
		Announce: NewReceiverAnnounce(config, storage),
		UDP:      NewReceiverUDP(config, storage),
	}
	return &receiver
}
