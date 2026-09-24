// Command agent is the ScrewLogger Windows agent. It runs in console mode by
// default (dev path, `-config agent.yaml`), or as the monsvc Windows service
// via the `-service`, `-install`, `-uninstall`, and `-upgrade` subcommands.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"screwlogger/internal/agent"
)

func main() {
	cfgPath := flag.String("config", "agent.yaml", "path to agent.yaml (console mode)")
	service := flag.Bool("service", false, "run as the monsvc Windows service (invoked by the SCM)")
	install := flag.Bool("install", false, "install and start the monsvc service")
	uninstall := flag.Bool("uninstall", false, "stop and remove the monsvc service")
	upgrade := flag.Bool("upgrade", false, "stop, replace (optional), and restart the monsvc service")
	serverURL := flag.String("server", "", "server URL (required by -install)")
	token := flag.String("token", "", "device token sl_... (required by -install)")
	dataDir := flag.String("data-dir", "", "data dir for -install (empty -> default)")
	newExe := flag.String("new", "", "replacement binary path for -upgrade")
	flag.Parse()

	switch {
	case *install:
		runInstall(*serverURL, *token, *dataDir)
	case *uninstall:
		runUninstall()
	case *upgrade:
		runUpgrade(*newExe)
	case *service:
		runService()
	default:
		runConsole(*cfgPath)
	}
}

// runConsole is the dev path: load -config, poll, buffer, and ship in the
// foreground until Ctrl-C / SIGTERM.
func runConsole(cfgPath string) {
	cfg, err := agent.LoadConfig(cfgPath)
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

// runService is the production path, invoked by the SCM. Config is read from
// agent.yaml next to the binary, not from -config.
func runService() {
	exe, _ := os.Executable()
	cfg, err := agent.LoadConfig(filepath.Join(filepath.Dir(exe), "agent.yaml"))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	fg, idle := agent.NewWin32Sources()
	if err := agent.RunService("monsvc", cfg, fg, idle, time.Now); err != nil {
		log.Fatalf("service: %v", err)
	}
}

// runInstall writes agent.yaml next to the binary and registers + starts the
// monsvc service. The token is never logged.
func runInstall(serverURL, token, dataDir string) {
	if serverURL == "" || token == "" {
		log.Fatal("install: -server and -token are required")
	}
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	if err := agent.WriteConfig(dir, serverURL, token, dataDir); err != nil {
		log.Fatalf("install: %v", err)
	}
	if err := agent.InstallService(exe, "monsvc", "ScrewLogger Monitor", "ScrewLogger usage monitor (SYSTEM)"); err != nil {
		fatalSCM("install", err)
	}
	if err := agent.StartService("monsvc"); err != nil {
		log.Fatalf("install: %v", err)
	}
	log.Printf("installed and started monsvc (server=%s)", serverURL)
}

// runUninstall stops and removes the monsvc service.
func runUninstall() {
	if err := agent.UninstallService("monsvc"); err != nil {
		fatalSCM("uninstall", err)
	}
	log.Printf("removed monsvc")
}

// runUpgrade stops, optionally replaces the binary, and restarts the monsvc
// service. agent.yaml (config + token) is untouched.
func runUpgrade(newExe string) {
	if err := agent.UpgradeService("monsvc", newExe); err != nil {
		fatalSCM("upgrade", err)
	}
	log.Printf("upgraded monsvc")
}

// fatalSCM exits with a clear elevation hint when the SCM rejects the request
// (access denied), or the underlying error otherwise.
func fatalSCM(op string, err error) {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		log.Fatalf("%s failed: run this installer from an elevated (Administrator) prompt", op)
	}
	log.Fatalf("%s: %v", op, err)
}
