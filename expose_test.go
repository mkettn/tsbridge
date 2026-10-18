package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loopbackTailnet fakes the tailnet node with loopback listeners on
// ephemeral ports and records where they ended up, since the port in the
// config is only meaningful on a real tailnet address.
type loopbackTailnet struct {
	tailnetListen
	tcp net.Listener
	udp net.PacketConn
	// gotNetwork/gotAddr are what startExpose asked the node to listen on.
	gotAddr string
}

func newLoopbackTailnet() *loopbackTailnet {
	lt := &loopbackTailnet{}
	lt.IP = netip.MustParseAddr("100.64.0.1")
	lt.Listen = func(network, addr string) (net.Listener, error) {
		lt.gotAddr = addr
		l, err := net.Listen("tcp", "127.0.0.1:0")
		lt.tcp = l
		return l, err
	}
	lt.ListenPacket = func(network, addr string) (net.PacketConn, error) {
		lt.gotAddr = addr
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		lt.udp = pc
		return pc, err
	}
	return lt
}

func shutdownNow(rb runningBridge) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rb.Shutdown(ctx)
}

// tcpEcho serves a TCP echo on l.
func tcpEcho(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); io.Copy(c, c) }()
	}
}

func roundTrip(t *testing.T, network, addr, msg string) string {
	t.Helper()
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func TestExpose_TCPToTCP(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go tcpEcho(backend)

	lt := newLoopbackTailnet()
	rb, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "web", Port: 8080, Mode: "tcp", Target: backend.Addr().String(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownNow(rb)

	if lt.gotAddr != ":8080" {
		t.Errorf("listened on %q, want :8080 (the node's own address)", lt.gotAddr)
	}
	if got := roundTrip(t, "tcp", lt.tcp.Addr().String(), "hello"); got != "hello" {
		t.Errorf("echo = %q", got)
	}
}

func TestExpose_TCPToUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.sock")
	backend, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go tcpEcho(backend)

	lt := newLoopbackTailnet()
	rb, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "svc", Port: 9000, Mode: "tcp", Target: path,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownNow(rb)

	if got := roundTrip(t, "tcp", lt.tcp.Addr().String(), "over a socket"); got != "over a socket" {
		t.Errorf("echo = %q", got)
	}
}

func TestExpose_HTTP(t *testing.T) {
	for _, unix := range []bool{false, true} {
		name := "tcp target"
		if unix {
			name = "unix target"
		}
		t.Run(name, func(t *testing.T) {
			network, addr := "tcp", "127.0.0.1:0"
			if unix {
				network, addr = "unix", filepath.Join(t.TempDir(), "app.sock")
			}
			backend, err := net.Listen(network, addr)
			if err != nil {
				t.Fatal(err)
			}
			var gotForwarded, gotPath string
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotForwarded, gotPath = r.Header.Get("X-Forwarded-For"), r.URL.Path
				io.WriteString(w, "hi from backend")
			})}
			go srv.Serve(backend)
			defer srv.Close()

			lt := newLoopbackTailnet()
			rb, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
				Name: "app", Port: 8080, Mode: "http", Target: backend.Addr().String(),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer shutdownNow(rb)

			resp, err := http.Get("http://" + lt.tcp.Addr().String() + "/some/path")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(body) != "hi from backend" {
				t.Errorf("got %d %q", resp.StatusCode, body)
			}
			if gotPath != "/some/path" {
				t.Errorf("backend saw path %q", gotPath)
			}
			if gotForwarded != "127.0.0.1" {
				t.Errorf("X-Forwarded-For = %q, want the client address", gotForwarded)
			}
		})
	}
}

func TestExpose_UDP(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := backend.ReadFrom(buf)
			if err != nil {
				return
			}
			backend.WriteTo(buf[:n], from)
		}
	}()

	lt := newLoopbackTailnet()
	rb, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "dns", Port: 5353, Mode: "udp", Target: backend.LocalAddr().String(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownNow(rb)

	if lt.gotAddr != "100.64.0.1:5353" {
		t.Errorf("listened on %q, want the node's tailnet IP and port", lt.gotAddr)
	}
	if got := roundTrip(t, "udp", lt.udp.LocalAddr().String(), "ping"); got != "ping" {
		t.Errorf("echo = %q", got)
	}
}

func TestExpose_UDPNeedsTailnetIP(t *testing.T) {
	lt := newLoopbackTailnet()
	lt.IP = netip.Addr{}
	_, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "dns", Port: 53, Mode: "udp", Target: "127.0.0.1:53",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "no tailnet address") {
		t.Errorf("err = %v", err)
	}
}

func TestExpose_ListenError(t *testing.T) {
	lt := newLoopbackTailnet()
	lt.Listen = func(string, string) (net.Listener, error) { return nil, io.ErrClosedPipe }
	_, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "web", Port: 80, Mode: "tcp", Target: "127.0.0.1:80",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckExpose(t *testing.T) {
	ok := ExposeConfig{Name: "a", Port: 80, Mode: "tcp", Target: "127.0.0.1:8080"}
	with := func(f func(*ExposeConfig)) ExposeConfig { e := ok; f(&e); return e }
	tests := []struct {
		name string
		list []ExposeConfig
		want string // substring of the error, "" for valid
	}{
		{"valid tcp host:port", []ExposeConfig{ok}, ""},
		{"valid unix socket", []ExposeConfig{with(func(e *ExposeConfig) { e.Target = "/run/a.sock" })}, ""},
		{"valid udp", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "udp" })}, ""},
		{"same port, different protocol", []ExposeConfig{ok, with(func(e *ExposeConfig) { e.Name = "b"; e.Mode = "udp" })}, ""},
		{"missing name", []ExposeConfig{with(func(e *ExposeConfig) { e.Name = "" })}, "name"},
		{"bad name", []ExposeConfig{with(func(e *ExposeConfig) { e.Name = "a b" })}, "invalid"},
		{"port zero", []ExposeConfig{with(func(e *ExposeConfig) { e.Port = 0 })}, "port"},
		{"port too big", []ExposeConfig{with(func(e *ExposeConfig) { e.Port = 70000 })}, "port"},
		{"valid http to socket", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "http"; e.Target = "/run/a.sock" })}, ""},
		{"valid https, tailnet cert", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "https" })}, ""},
		{"valid https, own cert", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "https"; e.TLSCert = "c"; e.TLSKey = "k" })}, ""},
		{"cert without key", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "https"; e.TLSCert = "c" })}, "together"},
		{"cert on http", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "http"; e.TLSCert = "c"; e.TLSKey = "k" })}, "only apply to mode: https"},
		{"bad mode", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "icmp" })}, "unsupported mode"},
		{"missing target", []ExposeConfig{with(func(e *ExposeConfig) { e.Target = "" })}, "target"},
		{"udp target is a path", []ExposeConfig{with(func(e *ExposeConfig) { e.Mode = "udp"; e.Target = "/run/a.sock" })}, "host:port"},
		{"target port zero", []ExposeConfig{with(func(e *ExposeConfig) { e.Target = "127.0.0.1:0" })}, "invalid port"},
		{"duplicate name", []ExposeConfig{ok, with(func(e *ExposeConfig) { e.Port = 81 })}, "duplicate expose name"},
		{"duplicate port", []ExposeConfig{ok, with(func(e *ExposeConfig) { e.Name = "b" })}, "duplicate expose port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := append([]ExposeConfig(nil), tt.list...)
			for i := range list {
				normalizeExpose(&list[i])
			}
			err := checkExpose(list)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadConfig_Expose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
expose:
  - name: web
    port: 8080
    target: run/web.sock
  - name: dns
    port: 53
    mode: UDP
    target: 127.0.0.1:5353
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Expose) != 2 {
		t.Fatalf("expose = %+v", cfg.Expose)
	}
	if got, want := cfg.Expose[0].Target, filepath.Join(dir, "run/web.sock"); got != want {
		t.Errorf("relative socket target = %q, want %q", got, want)
	}
	if cfg.Expose[0].Mode != "tcp" || cfg.Expose[1].Mode != "udp" {
		t.Errorf("modes = %q, %q", cfg.Expose[0].Mode, cfg.Expose[1].Mode)
	}
	if cfg.Expose[1].Target != "127.0.0.1:5353" {
		t.Errorf("host:port target changed: %q", cfg.Expose[1].Target)
	}
}

// writeCert writes a self-signed certificate for commonName to dir and
// returns the file paths.
func writeCert(t *testing.T, dir, commonName string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

// servedCN returns the common name of the certificate addr presents.
func servedCN(t *testing.T, addr string) string {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0].Subject.CommonName
}

func TestExpose_HTTPSWithCertificateFiles(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var gotTLS bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTLS = r.TLS != nil
		io.WriteString(w, "secure hello")
	})}
	go srv.Serve(backend)
	defer srv.Close()

	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, "first")
	lt := newLoopbackTailnet()
	rb, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "site", Port: 443, Mode: "https", Target: backend.Addr().String(),
		TLSCert: certFile, TLSKey: keyFile,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownNow(rb)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://" + lt.tcp.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secure hello" || gotTLS {
		t.Errorf("body %q, backend saw TLS=%v (want plain HTTP to the backend)", body, gotTLS)
	}

	// A renewed pair is served without a restart.
	if cn := servedCN(t, lt.tcp.Addr().String()); cn != "first" {
		t.Fatalf("cn = %q", cn)
	}
	writeCert(t, dir, "second")
	future := time.Now().Add(time.Minute)
	os.Chtimes(certFile, future, future)
	os.Chtimes(keyFile, future, future)
	if cn := servedCN(t, lt.tcp.Addr().String()); cn != "second" {
		t.Errorf("after renewal cn = %q, want second", cn)
	}

	// A broken replacement keeps the previous certificate in service.
	os.WriteFile(certFile, []byte("garbage"), 0o600)
	later := future.Add(time.Minute)
	os.Chtimes(certFile, later, later)
	if cn := servedCN(t, lt.tcp.Addr().String()); cn != "second" {
		t.Errorf("after bad reload cn = %q, want second", cn)
	}
}

func TestExpose_HTTPSTailnetCertificate(t *testing.T) {
	lt := newLoopbackTailnet()
	var asked string
	lt.ListenTLS = func(network, addr string) (net.Listener, error) {
		asked = addr
		return net.Listen("tcp", "127.0.0.1:0")
	}
	rb, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "site", Port: 443, Mode: "https", Target: "127.0.0.1:1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownNow(rb)
	if asked != ":443" {
		t.Errorf("ListenTLS addr = %q", asked)
	}
}

func TestExpose_HTTPSBadCertificate(t *testing.T) {
	lt := newLoopbackTailnet()
	_, err := startExpose(context.Background(), lt.tailnetListen, ExposeConfig{
		Name: "site", Port: 443, Mode: "https", Target: "127.0.0.1:1",
		TLSCert: "/nonexistent/cert.pem", TLSKey: "/nonexistent/key.pem",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "site") {
		t.Errorf("err = %v", err)
	}
}
