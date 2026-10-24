package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fl0v/retracker/internal/observability"
)

const VERSION = `0.10.1`

//go:embed favicon.ico
var faviconIco []byte

func helpText() {
	fmt.Println("# https://github.com/fl0v/retracker")
	flag.PrintDefaults()
}

func main() {
	registerConfigFlags(flag.CommandLine)
	ver := flag.Bool("v", false, "Show version")
	help := flag.Bool("h", false, "print this help")
	flag.Parse()

	if *help {
		helpText()
		os.Exit(0)
	}

	if *ver {
		fmt.Println(VERSION)
		os.Exit(0)
	}

	logger := observability.NewTextLogger(os.Stdout, false)
	logger.Info("Starting retracker", "version", VERSION)

	cfg, err := loadConfig(flag.CommandLine, os.LookupEnv)
	if err != nil {
		logger.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	logger = observability.NewTextLogger(os.Stdout, cfg.Debug)
	err = run(ctx, cfg, logger)
	stop()
	if err != nil {
		logger.Error("Tracker stopped", "error", err)
		os.Exit(1)
	}
}
