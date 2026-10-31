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
	"os"
	"os/signal"
	"strings"
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

// defaultConfigPath is used when neither -c nor -config is given.
const defaultConfigPath = "/etc/tsbridge/config.yaml"

// version is overwritten at build time for a release binary, via
// -ldflags "-X main.version=..." (see .github/workflows/release.yml). A
// plain `go build .` leaves it at "dev".
var version = "dev"

func run() error {
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "c", defaultConfigPath, "path to the YAML config file (shorthand for -config)")
	flag.StringVar(&configPath, "config", defaultConfigPath, "path to the YAML config file")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("tsbridge " + version)
		return nil
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}
	if len(cfg.Outbound) == 0 && len(cfg.Inbound) == 0 {
		log.Println("warning: no outbound or inbound entries configured, nothing to do")
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

	// srv.Up already succeeded above, so this always succeeds too (it
	// only errors if the server hasn't been started yet); lc.StatusWithoutPeers
	// is GET /status's data source, passed in the same way srv.Dial is.
	lc, err := srv.LocalClient()
	if err != nil {
		return fmt.Errorf("getting local client: %w", err)
	}

	manager := newBridgeManager(ctx, srv.Dial, cfg.SocketMode, cfg.SocketGroup, lc.StatusWithoutPeers)
	started, attempted, err := manager.startAll(cfg.Outbound)
	if err != nil {
		return err
	}

	log.Printf("running %d/%d bridge(s):", started, attempted)
	for _, b := range manager.List() {
		log.Printf("  - %s [%s]: %s -> %s", b.Name, b.Mode, b.Listen, strings.Join(b.Target, ", "))
	}

	// Only fatal if bridges were actually attempted and none survived --
	// zero configured to begin with (attempted == 0) is the existing
	// "nothing to do" warning above, not a startup failure.
	if attempted > 0 && started == 0 {
		return errors.New("no bridges could be started")
	}

	var inbound []runningBridge
	// Inbound ports are not optional the way one failing bridge is: with
	// no management API to show a failed one, a port that can't be opened
	// (typically one already taken) is reported by refusing to start.
	// Their own accept errors are logged where they happen, so no fatal
	// channel is needed.
	if len(cfg.Inbound) > 0 {
		tn, err := tailnetListener(srv)
		if err != nil {
			return err
		}
		for _, e := range cfg.Inbound {
			rb, err := startInbound(ctx, tn, e, nil)
			if err != nil {
				return err
			}
			inbound = append(inbound, rb)
			log.Printf("inbound %s [%s]: tailnet port %d -> %s", e.Name, e.Mode, e.Port, e.Target)
		}
	}

	var mgmt runningBridge
	if cfg.ManagementSocket != "" {
		mgmt, err = startManagementServer(cfg.ManagementSocket, cfg.ManagementSocketMode, cfg.ManagementSocketGroup, manager)
		if err != nil {
			return fmt.Errorf("starting management socket: %w", err)
		}
		log.Printf("management API listening on %s", cfg.ManagementSocket)
	}

	<-ctx.Done()
	// A bridge's own fatal error no longer reaches here: each bridge is
	// isolated (see manage.go's watchFatal) and only removes itself from
	// the running set, so the only thing that ends run() now is a signal.
	log.Println("shutting down...")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancelShutdown()
	// The management socket goes first so no request is in a handler while
	// bridges are being torn down.
	if mgmt != nil {
		mgmt.Shutdown(shutdownCtx)
	}
	for _, rb := range inbound {
		rb.Shutdown(shutdownCtx)
	}
	manager.Shutdown(shutdownCtx)

	log.Println("shutdown complete")
	return nil
}

// tailnetListener adapts the running tsnet node for startInbound.
func tailnetListener(srv *tsnet.Server) (tailnetListen, error) {
	ip4, ip6 := srv.TailscaleIPs()
	ip := ip4
	if !ip.IsValid() {
		ip = ip6
	}
	if !ip.IsValid() {
		return tailnetListen{}, errors.New("inbound: node has no tailnet address")
	}
	return tailnetListen{Listen: srv.Listen, ListenPacket: srv.ListenPacket, ListenTLS: srv.ListenTLS, IP: ip}, nil
}
