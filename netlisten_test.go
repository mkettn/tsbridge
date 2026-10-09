package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().String()
}

func TestStartBridge_TCPListenForwards(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c) // echo
	}()

	ctx, cancel := context.WithCancel(context.Background())
	addr := freeTCPAddr(t)
	b := BridgeConfig{Name: "tcp-listen", Listen: addr, Targets: Targets{"example.invalid:1"}, Mode: "tcp"}
	rb, err := startBridge(ctx, dialToAddr(backend.Addr().String()), b, 0660, "", make(chan error, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, cancel, rb)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("got %q, %v; want ping", got, err)
	}
}

func TestStartBridge_UDPListenForwardsPerClient(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() { // echo, prefixed so we know it came through the backend
		buf := make([]byte, 1500)
		for {
			n, from, err := backend.ReadFrom(buf)
			if err != nil {
				return
			}
			backend.WriteTo(append([]byte("echo:"), buf[:n]...), from)
		}
	}()

	var gotNetwork string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		gotNetwork = network
		var d net.Dialer
		return d.DialContext(ctx, "udp", backend.LocalAddr().String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	addr := freeUDPAddr(t)
	b := BridgeConfig{Name: "udp-listen", Listen: addr, Targets: Targets{"example.invalid:1"}, Mode: "udp"}
	rb, err := startBridge(ctx, dial, b, 0660, "", make(chan error, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, cancel, rb)

	// Two clients must each get their own reply.
	for _, msg := range []string{"one", "two"} {
		c, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		c.Close()
		if err != nil || string(buf[:n]) != "echo:"+msg {
			t.Fatalf("client %s: got %q, %v", msg, buf[:n], err)
		}
	}
	if gotNetwork != "udp" {
		t.Fatalf("dialed network %q; want udp", gotNetwork)
	}
}

func TestUDPBridge_IdleSessionExpires(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", backend.LocalAddr().String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := freeUDPAddr(t)
	rb, err := startUDPBridge(ctx, dial, BridgeConfig{Name: "idle", Targets: Targets{"x:1"}}, addr, 50*time.Millisecond, make(chan error, 1))
	if err != nil {
		t.Fatal(err)
	}
	u := rb.(*udpBridge)
	defer shutdown(t, cancel, rb)

	c, _ := net.Dial("udp", addr)
	defer c.Close()
	c.Write([]byte("hi"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		n := len(u.sessions)
		u.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for time.Now().Before(deadline) {
		u.mu.Lock()
		n := len(u.sessions)
		u.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("idle udp session was never reaped")
}

func TestParseListen(t *testing.T) {
	tests := []struct {
		listen, mode, network string
	}{
		{"127.0.0.1:8080", "tcp", "tcp"},
		{"127.0.0.1:8080", "http", "tcp"},
		{"[::1]:8080", "tcp", "tcp"},
		{"127.0.0.1:5353", "udp", "udp"},
		{"/run/x.sock", "tcp", "unix"},
		{"0.0.0.0:8080", "tcp", "tcp"},
		{"localhost:8080", "http", "tcp"},
		{"/run/x:80", "tcp", "unix"},
		{"svc.socket:80", "tcp", "unix"},
		{"./SVC.socket:80", "tcp", "unix"},
		{":8080", "tcp", "unix"},
		{"::1:8080", "tcp", "tcp"},
		{"fe80::1:8080", "tcp", "tcp"},
		{"/run/a:b:c.sock", "tcp", "unix"},
		{"run/a:b:c.sock", "tcp", "unix"},
		{"./a:b:c.sock", "tcp", "unix"},
		{"a:b:c.sock", "tcp", "tcp"}, // documented limit: taken for bare IPv6
		{"x.sock", "http", "unix"},
	}
	for _, tc := range tests {
		got, _ := parseListen(BridgeConfig{Listen: tc.listen, Mode: tc.mode})
		if got != tc.network {
			t.Errorf("parseListen(%q, %s) = %s; want %s", tc.listen, tc.mode, got, tc.network)
		}
	}
}

func TestValidateBridgeFields_NetworkListen(t *testing.T) {
	ok := BridgeConfig{Name: "n", Targets: Targets{"t:1"}, Mode: "tcp"}
	tests := []struct {
		name    string
		mutate  func(*BridgeConfig)
		wantErr string // "" = valid
	}{
		{"tcp ok", func(b *BridgeConfig) { b.Listen = "127.0.0.1:8080" }, ""},
		{"udp ok", func(b *BridgeConfig) { b.Listen = "127.0.0.1:5353"; b.Mode = "udp" }, ""},
		{"http on port ok", func(b *BridgeConfig) { b.Listen = "127.0.0.1:8080"; b.Mode = "http" }, ""},
		{"udp no port", func(b *BridgeConfig) { b.Listen = "127.0.0.1"; b.Mode = "udp" }, "must be host:port"},
		{"udp hostname", func(b *BridgeConfig) { b.Listen = "svc.socket:53"; b.Mode = "udp" }, "must be a bind address"},
		{"udp localhost ok", func(b *BridgeConfig) { b.Listen = "localhost:53"; b.Mode = "udp" }, ""},
		{"udp path", func(b *BridgeConfig) { b.Listen = "/run/x.sock"; b.Mode = "udp" }, "must be host:port"},
		{"unbracketed ipv6", func(b *BridgeConfig) { b.Listen = "::1:8080" }, "IPv6 needs brackets"},
		{"udp zero port", func(b *BridgeConfig) { b.Listen = "127.0.0.1:0"; b.Mode = "udp" }, "invalid port"},
		{"socket_mode on tcp", func(b *BridgeConfig) { b.Listen = "127.0.0.1:80"; b.SocketMode = "0660" }, "only apply to a Unix socket"},
		{"socket_group on udp", func(b *BridgeConfig) { b.Listen = "127.0.0.1:53"; b.Mode = "udp"; b.SocketGroup = "x" }, "only apply to a Unix socket"},
		{"rewrite_host on udp", func(b *BridgeConfig) { b.Listen = "127.0.0.1:53"; b.Mode = "udp"; b.RewriteHost = true }, "rewrite_host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := ok
			tc.mutate(&b)
			err := validateBridgeFields(b)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v; want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckBridges_TCPAndUDPMayShareAddress(t *testing.T) {
	tcp := BridgeConfig{Name: "dns-tcp", Listen: "127.0.0.1:5353", Targets: Targets{"ns:53"}, Mode: "tcp"}
	udp := BridgeConfig{Name: "dns-udp", Listen: "127.0.0.1:5353", Targets: Targets{"ns:53"}, Mode: "udp"}
	if err := checkBridges([]BridgeConfig{tcp, udp}); err != nil {
		t.Fatalf("tcp+udp on the same address should load: %v", err)
	}
	dup := tcp
	dup.Name = "dns-tcp2"
	if err := checkBridges([]BridgeConfig{tcp, dup}); err == nil || !strings.Contains(err.Error(), "duplicate listen address") {
		t.Fatalf("two tcp binds of one address must collide, got: %v", err)
	}
}

func TestRoundRobin(t *testing.T) {
	rr := newRoundRobin([]string{"a", "b", "c"})
	var got []string
	for i := 0; i < 7; i++ {
		got = append(got, rr.next())
	}
	if want := "a b c a b c a"; strings.Join(got, " ") != want {
		t.Errorf("got %v, want %s", got, want)
	}
}

func TestTargets_Unmarshal(t *testing.T) {
	var y struct {
		T Targets `yaml:"target"`
	}
	for in, want := range map[string]string{
		"target: [a:1, b:2]":        "a:1 b:2",
		"target: a:1":               "a:1",
		"target:\n  - a:1\n  - b:2": "a:1 b:2",
	} {
		y.T = nil
		if err := yaml.Unmarshal([]byte(in), &y); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := strings.Join(y.T, " "); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	if err := yaml.Unmarshal([]byte("target: {a: b}"), &y); err == nil {
		t.Error("mapping target should be rejected")
	}

	for in, want := range map[string]string{
		`{"target":["a:1","b:2"]}`: "a:1 b:2",
		`{"target":"a:1"}`:         "a:1",
	} {
		var b BridgeConfig
		if err := json.Unmarshal([]byte(in), &b); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := strings.Join(b.Targets, " "); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
	out, _ := json.Marshal(BridgeConfig{Targets: Targets{"a:1"}})
	if !strings.Contains(string(out), `"target":["a:1"]`) {
		t.Errorf("marshal should emit a list: %s", out)
	}
}

func TestValidateBridgeFields_Targets(t *testing.T) {
	b := BridgeConfig{Name: "n", Listen: "/x.sock", Mode: "tcp"}
	if err := validateBridgeFields(b); err == nil {
		t.Error("no targets should be rejected")
	}
	b.Targets = Targets{"a:1", " "}
	if err := validateBridgeFields(b); err == nil {
		t.Error("blank target should be rejected")
	}
	b.Targets = Targets{"a:1", "b:2"}
	if err := validateBridgeFields(b); err != nil {
		t.Errorf("valid targets rejected: %v", err)
	}
}

func TestTCPBridge_RoundRobinTargets(t *testing.T) {
	var mu sync.Mutex
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return nil, errors.New("no route")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := BridgeConfig{Name: "rr", Mode: "tcp", Targets: Targets{"a:1", "b:2"}}
	rb := startTCPBridge(ctx, dial, b, l, make(chan error, 1))
	for i := 0; i < 4; i++ {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, c) // returns when the bridge closes it after the failed dial
		c.Close()
	}
	rb.Shutdown(ctx)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(dialed, " "); got != "a:1 b:2 a:1 b:2" {
		t.Errorf("dialed %q", got)
	}
}

func TestHTTPBridge_RoundRobinTargets(t *testing.T) {
	var mu sync.Mutex
	var dialed []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return nil, errors.New("no route")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := BridgeConfig{Name: "rr-http", Mode: "http", Targets: Targets{"a:1", "b:2"}}
	rb := startHTTPBridge(ctx, dial, b, l, make(chan error, 1))
	defer rb.Shutdown(ctx)
	for i := 0; i < 4; i++ {
		resp, err := http.Get("http://" + l.Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(dialed, " "); got != "a:1 b:2 a:1 b:2" {
		t.Errorf("dialed %q", got)
	}
}
