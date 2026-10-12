// Command linux-dash-agent runs on each Linux host, reports metrics, apps,
// errors and pending updates to the Linux Dash control plane, and executes
// allowlisted commands it receives back.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	_ "time/tzdata" // maintenance-window timezones work on hosts without zoneinfo

	"linux-dash-agent/internal/agent"
	"linux-dash-agent/internal/config"
	"linux-dash-agent/internal/state"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `linux-dash-agent %s

Usage:
  linux-dash-agent run    [-config PATH]            run the agent (default)
  linux-dash-agent check  [-config PATH] [-updates] collect once and print the report as JSON
  linux-dash-agent validate [-config PATH]          check the config file and exit
  linux-dash-agent version

Default config: %s
`, version, config.DefaultPath)
}

func main() {
	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	cfgPath := fs.String("config", config.DefaultPath, "path to config file")
	withUpdates := fs.Bool("updates", false, "check: also refresh and list package updates (needs root)")
	fs.Parse(args)

	switch cmd {
	case "version":
		fmt.Println(version)
		return
	case "run", "check", "validate":
	default:
		usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if cmd == "validate" {
		fmt.Println("config ok:", *cfgPath)
		return
	}
	log := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cmd == "check" {
		dir, err := os.MkdirTemp("", "linux-dash-agent-check")
		if err != nil {
			fatal(log, err)
		}
		defer os.RemoveAll(dir)
		store, err := state.Open(dir)
		if err != nil {
			fatal(log, err)
		}
		a, err := agent.New(cfg, store, log, version)
		if err != nil {
			fatal(log, err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(a.Snapshot(ctx, *withUpdates))
		return
	}

	store, err := state.Open(cfg.StateDir)
	if err != nil {
		fatal(log, fmt.Errorf("opening state dir: %w", err))
	}
	a, err := agent.New(cfg, store, log, version)
	if err != nil {
		fatal(log, err)
	}
	a.ConfigPath = *cfgPath
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		if errors.Is(err, agent.ErrRestart) {
			log.Info("exiting for restart")
			os.Exit(0) // systemd (Restart=always) / OpenRC start the new binary
		}
		fatal(log, err)
	}
	log.Info("agent stopped")
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func fatal(log *slog.Logger, err error) {
	log.Error(err.Error())
	os.Exit(1)
}
