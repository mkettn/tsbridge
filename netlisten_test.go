package main

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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
	b := BridgeConfig{Name: "tcp-listen", Listen: addr, Target: "example.invalid:1", Mode: "tcp"}
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
	b := BridgeConfig{Name: "udp-listen", Listen: addr, Target: "example.invalid:1", Mode: "udp"}
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
	rb, err := startUDPBridge(ctx, dial, BridgeConfig{Name: "idle", Target: "x:1"}, addr, make(chan error, 1))
	if err != nil {
		t.Fatal(err)
	}
	u := rb.(*udpBridge)
	u.idle = 50 * time.Millisecond
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
	ok := BridgeConfig{Name: "n", Target: "t:1", Mode: "tcp"}
	tests := []struct {
		name    string
		mutate  func(*BridgeConfig)
		wantErr string // "" = valid
	}{
		{"tcp ok", func(b *BridgeConfig) { b.Listen = "127.0.0.1:8080" }, ""},
		{"udp ok", func(b *BridgeConfig) { b.Listen = "127.0.0.1:5353"; b.Mode = "udp" }, ""},
		{"http on port ok", func(b *BridgeConfig) { b.Listen = "127.0.0.1:8080"; b.Mode = "http" }, ""},
		{"udp no port", func(b *BridgeConfig) { b.Listen = "127.0.0.1"; b.Mode = "udp" }, "must be host:port"},
		{"udp path", func(b *BridgeConfig) { b.Listen = "/run/x.sock"; b.Mode = "udp" }, "must be host:port"},
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
