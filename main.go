// Command tsbridge joins a Tailscale tailnet in userspace (via tsnet, no
// TUN device or system-wide network interface) and exposes one or more
// tailnet TCP services locally as Unix sockets.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

// shutdownDrainTimeout bounds how long we wait for in-flight connections
// to finish copying on SIGTERM/SIGINT before giving up and exiting
// anyway (systemd's default TimeoutStopSec is 90s, so this leaves
// plenty of margin before it would SIGKILL us).
const shutdownDrainTimeout = 10 * time.Second

func main() {
	log.SetFlags(0)
	log.SetPrefix("tsbridge: ")

	// run, not main, does the work: main() is the only place allowed to
	// exit the process, so every defer inside run() (notably
	// srv.Close(), which logs the tsnet node out) actually executes
	// before we exit non-zero.
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/tsbridge/config.yaml", "path to the main YAML config file")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}
	if len(cfg.Bridges) == 0 {
		log.Println("warning: no bridges configured, nothing to do")
	}

	// TS_AUTHKEY is optional: pointing tsbridge at a control server (the
	// Tailscale default, or control_url for a self-hosted Headscale) is
	// enough on its own. With no auth key, tsnet drives registration
	// itself and logs a one-time URL below to approve this node -- no
	// separate registration step outside tsbridge. TS_AUTHKEY remains the
	// way to skip that interactive step for unattended/first-boot setups
	// (a Tailscale auth key or a Headscale preauth key both work).
	authKey := os.Getenv("TS_AUTHKEY")
	if authKey == "" {
		log.Println("no TS_AUTHKEY set: watch for a registration URL logged below and open it once to approve this node with the control server")
	}

	srv := &tsnet.Server{
		Hostname:   cfg.Hostname,
		Dir:        cfg.StateDir,
		Ephemeral:  cfg.Ephemeral,
		ControlURL: cfg.ControlURL,
		AuthKey:    authKey,
		UserLogf:   log.Printf,
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if _, err := srv.Up(ctx); err != nil {
		return fmt.Errorf("joining tailnet: %w", err)
	}
	controlServer := cfg.ControlURL
	if controlServer == "" {
		controlServer = "default (Tailscale)"
	}
	log.Printf("joined tailnet as %q via %s (ephemeral=%v, state_dir=%q)", cfg.Hostname, controlServer, cfg.Ephemeral, cfg.StateDir)

	log.Printf("resolved %d bridge(s):", len(cfg.Bridges))
	for _, b := range cfg.Bridges {
		log.Printf("  - %s: %s -> %s [%s]", b.Name, b.Listen, b.Target, b.Source)
	}

	var wg sync.WaitGroup
	var listeners []net.Listener
	// Buffered so a bridge's accept loop can report a fatal error and
	// return without blocking on a reader that may already have moved
	// on to shutdown.
	fatal := make(chan error, len(cfg.Bridges))
	for _, b := range cfg.Bridges {
		l, err := startBridge(ctx, srv, b, cfg.SocketMode, cfg.SocketGroup, &wg, fatal)
		if err != nil {
			log.Printf("bridge %s: failed to start, skipping: %v", b.Name, err)
			continue
		}
		listeners = append(listeners, l)
	}

	if len(cfg.Bridges) > 0 && len(listeners) == 0 {
		return errors.New("no bridges could be started")
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-fatal:
		// A bridge's accept loop died for good (not just shutting
		// down); take the whole process down so Restart=on-failure
		// actually fires instead of leaving a silently dead bridge.
		runErr = err
		stop()
	}
	log.Println("shutting down...")

	for _, l := range listeners {
		l.Close() // unlinks the Unix socket file
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownDrainTimeout):
		log.Printf("shutdown: %v drain timeout reached with connections still open, exiting anyway", shutdownDrainTimeout)
	}

	log.Println("shutdown complete")
	return runErr
}
