// Command tsbridge joins a Tailscale tailnet in userspace (via tsnet, no
// TUN device or system-wide network interface) and exposes one or more
// tailnet TCP services locally as Unix sockets.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"tailscale.com/tsnet"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("tsbridge: ")

	configPath := flag.String("config", "/etc/tsbridge/config.yaml", "path to the main YAML config file")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if len(cfg.Bridges) == 0 {
		log.Println("warning: no bridges configured, nothing to do")
	}

	authKey := os.Getenv("TS_AUTHKEY")
	if authKey == "" {
		log.Println("warning: TS_AUTHKEY is not set; tsnet will require interactive login on first run")
	}

	srv := &tsnet.Server{
		Hostname:  cfg.Hostname,
		Dir:       cfg.StateDir,
		Ephemeral: cfg.Ephemeral,
		AuthKey:   authKey,
		UserLogf:  log.Printf,
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if _, err := srv.Up(ctx); err != nil {
		log.Fatalf("joining tailnet: %v", err)
	}
	log.Printf("joined tailnet as %q (ephemeral=%v, state_dir=%q)", cfg.Hostname, cfg.Ephemeral, cfg.StateDir)

	log.Printf("resolved %d bridge(s):", len(cfg.Bridges))
	for _, b := range cfg.Bridges {
		log.Printf("  - %s: %s -> %s [%s]", b.Name, b.Listen, b.Target, b.Source)
	}

	var wg sync.WaitGroup
	var listeners []net.Listener
	for _, b := range cfg.Bridges {
		l, err := startBridge(ctx, srv, b, cfg.SocketMode, cfg.SocketGroup, &wg)
		if err != nil {
			log.Printf("bridge %s: failed to start, skipping: %v", b.Name, err)
			continue
		}
		listeners = append(listeners, l)
	}

	if len(cfg.Bridges) > 0 && len(listeners) == 0 {
		log.Fatalf("no bridges could be started")
	}

	<-ctx.Done()
	log.Println("shutting down...")

	for _, l := range listeners {
		l.Close() // unlinks the Unix socket file
	}
	wg.Wait()

	log.Println("shutdown complete")
}
