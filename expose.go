package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// ExposeConfig is one tailnet-port -> local-address mapping, the schema of
// an entry in config.yaml's expose: list. It is the opposite direction of a
// bridge: traffic reaching this node's tailnet address on Port is forwarded
// to Target on the host.
type ExposeConfig struct {
	Name string `yaml:"name"`
	// Port is the port on this node's tailnet address to accept on. Who
	// on the tailnet may reach it is up to the tailnet's ACLs.
	Port int `yaml:"port"`
	// Mode is "tcp" (the default), "http" or "udp". It decides what the
	// tailnet port is and what Target may be: tcp forwards a byte stream
	// and http reverse-proxies each request, both to a Unix socket path or
	// a host:port; udp relays datagrams to a host:port (UDP has no
	// sockets-by-path).
	Mode string `yaml:"mode"`
	// Target is where the host side lives, written like a bridge's listen:
	// an IP literal or localhost plus port, otherwise a Unix socket path
	// (relative paths resolve against the config file's directory).
	Target string `yaml:"target"`
}

var supportedExposeModes = map[string]bool{"tcp": true, "http": true, "udp": true}

func normalizeExpose(e *ExposeConfig) {
	e.Mode = strings.ToLower(strings.TrimSpace(e.Mode))
	if e.Mode == "" {
		e.Mode = defaultBridgeMode
	}
	e.Target = strings.TrimSpace(e.Target)
}

func validateExposeFields(e ExposeConfig) error {
	if e.Name == "" {
		return fmt.Errorf("an expose entry is missing required field 'name'")
	}
	if !validBridgeName(e.Name) {
		return fmt.Errorf("expose name %q is invalid: use only letters, digits, '_', '.' and '-' (and not just dots)", e.Name)
	}
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("expose %q: port must be between 1 and 65535", e.Name)
	}
	if !supportedExposeModes[e.Mode] {
		return fmt.Errorf("expose %q has unsupported mode %q; supported modes are tcp, http, udp", e.Name, e.Mode)
	}
	if e.Target == "" {
		return fmt.Errorf("expose %q is missing required field 'target'", e.Name)
	}
	if network, addr := parseEndpoint(e.Mode, e.Target); network != "unix" {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf("expose %q: target %q must be host:port (e.g. 127.0.0.1:8080; IPv6 needs brackets: [::1]:8080)", e.Name, e.Target)
		}
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return fmt.Errorf("expose %q: target %q has an invalid port", e.Name, e.Target)
		}
		if !isBindHost(host) {
			return fmt.Errorf("expose %q: target %q must be an IP literal or localhost, then a port (e.g. 127.0.0.1:%s)", e.Name, e.Target, port)
		}
	}
	return nil
}

// checkExpose validates each entry and rejects duplicate names, or two
// entries claiming the same port on the same protocol (tcp and udp are
// separate port spaces).
func checkExpose(list []ExposeConfig) error {
	names := map[string]bool{}
	ports := map[string]bool{}
	for _, e := range list {
		if err := validateExposeFields(e); err != nil {
			return err
		}
		if names[e.Name] {
			return fmt.Errorf("duplicate expose name %q", e.Name)
		}
		names[e.Name] = true
		key := e.Mode + "!" + strconv.Itoa(e.Port)
		if ports[key] {
			return fmt.Errorf("duplicate expose port %d on %s", e.Port, e.Mode)
		}
		ports[key] = true
	}
	return nil
}

// tailnetListen is what expose needs from the tailnet node: in production
// the methods of a *tsnet.Server, in tests loopback fakes.
type tailnetListen struct {
	Listen       func(network, addr string) (net.Listener, error)
	ListenPacket func(network, addr string) (net.PacketConn, error)
	// IP is the node's own tailnet address; tsnet's ListenPacket, unlike
	// Listen, needs one spelled out.
	IP netip.Addr
}

// localDialer dials a fixed local address whatever the bridge machinery
// asks for, so expose can reuse the same tcp/udp relay code as bridges: the
// "target" those pass around is only a label here.
func localDialer(network, addr string) dialFunc {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

// startExpose accepts on the tailnet port and relays each connection (tcp)
// or client session (udp) to the local target, until Shutdown.
func startExpose(ctx context.Context, tn tailnetListen, e ExposeConfig, fatal chan<- error) (runningBridge, error) {
	network, addr := parseEndpoint(e.Mode, e.Target)
	dial := localDialer(network, addr)
	// The relay code logs b.Listen as the source and dials b.Targets.
	b := BridgeConfig{
		Name:    e.Name,
		Listen:  "tailnet:" + strconv.Itoa(e.Port),
		Targets: Targets{e.Target},
		Mode:    e.Mode,
	}
	switch e.Mode {
	case "udp":
		if !tn.IP.IsValid() {
			return nil, fmt.Errorf("expose %s: node has no tailnet address to listen on", e.Name)
		}
		pc, err := tn.ListenPacket("udp", netip.AddrPortFrom(tn.IP, uint16(e.Port)).String())
		if err != nil {
			return nil, fmt.Errorf("expose %s: listening on tailnet udp %d: %w", e.Name, e.Port, err)
		}
		return serveUDP(ctx, dial, b, pc, udpSessionIdleTimeout, fatal), nil
	default: // tcp and http both accept a stream on the tailnet port
		l, err := tn.Listen("tcp", ":"+strconv.Itoa(e.Port))
		if err != nil {
			return nil, fmt.Errorf("expose %s: listening on tailnet tcp %d: %w", e.Name, e.Port, err)
		}
		return startListener(ctx, dial, b, l, fatal), nil
	}
}
