package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultSocketMode os.FileMode = 0660

// defaultBridgeMode is used when a bridge doesn't set mode:.
const defaultBridgeMode = "tcp"

// supportedBridgeModes are the values checkBridges accepts for mode:.
var supportedBridgeModes = map[string]bool{
	"tcp":  true, // raw bidirectional byte copy
	"http": true, // terminate HTTP and reverse-proxy to target
}

// BridgeConfig is one listen-socket -> tailnet-target mapping.
type BridgeConfig struct {
	Name   string `yaml:"name"`
	Listen string `yaml:"listen"`
	Target string `yaml:"target"`
	// Mode selects how tsbridge handles the connection, not what network
	// it dials on the tailnet side -- that's always TCP regardless of
	// Mode. "tcp" (the default if unset) does a raw bidirectional byte
	// copy; "http" terminates HTTP on the socket and reverse-proxies
	// each request to Target instead.
	Mode string `yaml:"mode"`
	// RewriteHost only applies to mode: http (rejected on any other
	// mode). false (the default) forwards the request to Target with
	// whatever Host header the client sent unchanged. true rewrites it
	// to Target's own host:port instead -- needed for a target that
	// routes or validates by hostname (tailscale serve, notably).
	RewriteHost bool `yaml:"rewrite_host"`
}

// Config is the fully resolved, validated configuration used at runtime.
type Config struct {
	Hostname    string
	StateDir    string
	Ephemeral   bool
	ControlURL  string
	SocketGroup string
	SocketMode  os.FileMode
	Bridges     []BridgeConfig
}

// rawConfig mirrors the config.yaml schema.
type rawConfig struct {
	Hostname    string         `yaml:"hostname"`
	StateDir    string         `yaml:"state_dir"`
	Ephemeral   *bool          `yaml:"ephemeral"`
	ControlURL  string         `yaml:"control_url"`
	SocketGroup string         `yaml:"socket_group"`
	SocketMode  string         `yaml:"socket_mode"`
	Bridges     []BridgeConfig `yaml:"bridges"`
}

// LoadConfig reads and validates the config file at path. Any error
// returned here is fatal: callers should log it and exit non-zero.
func LoadConfig(path string) (*Config, error) {
	raw, err := readRawConfig(path)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}

	baseDir := filepath.Dir(path)
	bridges := raw.Bridges
	for i := range bridges {
		bridges[i].Listen = resolvePath(baseDir, bridges[i].Listen)
		bridges[i].Mode = strings.ToLower(strings.TrimSpace(bridges[i].Mode))
		if bridges[i].Mode == "" {
			bridges[i].Mode = defaultBridgeMode
		}
	}

	if err := checkBridges(bridges); err != nil {
		return nil, err
	}

	mode := defaultSocketMode
	if raw.SocketMode != "" {
		mode, err = parseSocketMode(raw.SocketMode)
		if err != nil {
			return nil, fmt.Errorf("socket_mode: %w", err)
		}
	}

	if raw.ControlURL != "" {
		if err := validateControlURL(raw.ControlURL); err != nil {
			return nil, fmt.Errorf("control_url: %w", err)
		}
	}

	ephemeral := false
	if raw.Ephemeral != nil {
		ephemeral = *raw.Ephemeral
	}

	hostname := raw.Hostname
	if hostname == "" {
		hostname = "tsbridge"
	}

	return &Config{
		Hostname:    hostname,
		StateDir:    resolvePath(baseDir, raw.StateDir),
		Ephemeral:   ephemeral,
		ControlURL:  raw.ControlURL,
		SocketGroup: raw.SocketGroup,
		SocketMode:  mode,
		Bridges:     bridges,
	}, nil
}

// resolvePath returns p unchanged if it's empty or already absolute;
// otherwise it's resolved relative to baseDir, the config file's own
// directory.
func resolvePath(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

func readRawConfig(path string) (*rawConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var cfg rawConfig
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	return &cfg, nil
}

// checkBridges validates required fields and rejects duplicate names or
// listen paths.
func checkBridges(bridges []BridgeConfig) error {
	names := map[string]bool{}
	listens := map[string]bool{}
	for _, b := range bridges {
		if b.Name == "" {
			return fmt.Errorf("a bridge is missing required field 'name'")
		}
		if b.Listen == "" {
			return fmt.Errorf("bridge %q is missing required field 'listen'", b.Name)
		}
		if b.Target == "" {
			return fmt.Errorf("bridge %q is missing required field 'target'", b.Name)
		}
		if !supportedBridgeModes[b.Mode] {
			return fmt.Errorf("bridge %q has unsupported mode %q; supported modes are tcp, http", b.Name, b.Mode)
		}
		if b.RewriteHost && b.Mode != "http" {
			return fmt.Errorf("bridge %q sets rewrite_host, but that only applies to mode: http (bridge is mode: %s)", b.Name, b.Mode)
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate bridge name %q", b.Name)
		}
		names[b.Name] = true
		if listens[b.Listen] {
			return fmt.Errorf("duplicate listen path %q", b.Listen)
		}
		listens[b.Listen] = true
	}
	return nil
}

// validateControlURL rejects a control_url that isn't a usable absolute
// URL before it ever reaches tsnet.Server.ControlURL, where the same
// problem either fails silently (an unreachable/malformed value) or
// registers over plaintext. control_url decides which server this node
// trusts for registration and policy, so http:// is rejected outright
// rather than merely warned about.
func validateControlURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("must be an absolute URL, e.g. https://headscale.example.com (got %q)", raw)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must use https, got scheme %q -- a plaintext control server exposes registration and policy to tampering", u.Scheme)
	}
	return nil
}

func parseSocketMode(s string) (os.FileMode, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q (expected octal, e.g. \"0660\"): %w", s, err)
	}
	return os.FileMode(v), nil
}
