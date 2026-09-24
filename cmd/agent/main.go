// Command agent is the ScrewLogger Windows agent in console mode (M1).
// M3 wraps this as the monsvc Windows service.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"screwlogger/internal/agent"
)

func main() {
	cfgPath := flag.String("config", "agent.yaml", "path to agent.yaml")
	flag.Parse()

	cfg, err := agent.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	fg, idle := agent.NewWin32Sources()
	a, err := agent.New(cfg, fg, idle, nil)
	if err != nil {
		log.Fatalf("agent: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("agent running: server=%s data=%s", cfg.ServerURL, cfg.DataDir)
	if err := a.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}
	log.Printf("agent stopped cleanly")
}
