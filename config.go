package main

import (
	"fmt"
	"os"

	"github.com/vvampirius/retracker/common"
	"gopkg.in/yaml.v2"
)

type Config struct {
	AnnounceResponseInterval int
	Listen                   string
	UDPListen                string
	Debug                    bool
	Age                      float64
	XRealIP                  bool
	Forwards                 []common.Forward
	ForwardTimeout           int
}

func (config *Config) ReloadForwards(fileName string) error {
	f, err := os.Open(fileName)
	if err != nil {
		ErrorLog.Printf("Failed to open forwarders file '%s': %s\n", fileName, err.Error())
		return err
	}
	defer f.Close()
	forwards := make([]common.Forward, 0)
	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(&forwards); err != nil {
		ErrorLog.Printf("Failed to parse forwarders file '%s': %s\n", fileName, err.Error())
		return err
	}
	config.Forwards = forwards
	DebugLog.Printf("Loaded %d forwarder(s) from '%s'\n", len(forwards), fileName)
	for i, forward := range forwards {
		forwardName := forward.GetName()
		forwardInfo := fmt.Sprintf("  [%d] %s", i+1, forwardName)
		if forward.Uri != "" {
			forwardInfo += fmt.Sprintf(" -> %s", forward.Uri)
		}
		if forward.Ip != "" {
			forwardInfo += fmt.Sprintf(" (IP: %s)", forward.Ip)
		}
		if forward.Host != "" {
			forwardInfo += fmt.Sprintf(" (Host: %s)", forward.Host)
		}
		DebugLog.Println(forwardInfo)
	}
	return nil
}
