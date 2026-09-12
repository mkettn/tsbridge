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

// defaultManagementSocketMode is more restrictive than defaultSocketMode:
// the management socket can create and remove bridges to arbitrary
// tailnet targets, a much bigger blast radius than any single bridge
// socket, so it defaults to owner-only rather than owner+group.
const defaultManagementSocketMode os.FileMode = 0600

// defaultBridgeMode is used when a bridge doesn't set mode:.
const defaultBridgeMode = "tcp"

// managedBridgesFileName is the state_dir file the management API
// persists its own bridges to -- see manage.go.
const managedBridgesFileName = "managed-bridges.yaml"

// supportedBridgeModes are the values checkBridges accepts for mode:.
var supportedBridgeModes = map[string]bool{
	"tcp":  true, // raw bidirectional byte copy
	"http": true, // terminate HTTP and reverse-proxy to target
}

// BridgeConfig is one listen-socket -> tailnet-target mapping. It doubles
// as the management API's JSON request/response body and the schema of
// managed-bridges.yaml (see manage.go) -- both are "just another bridges:
// list" in the same shape as config.yaml's.
type BridgeConfig struct {
	Name   string `yaml:"name" json:"name"`
	Listen string `yaml:"listen" json:"listen"`
	Target string `yaml:"target" json:"target"`
	// Mode selects how tsbridge handles the connection, not what network
	// it dials on the tailnet side -- that's always TCP regardless of
	// Mode. "tcp" (the default if unset) does a raw bidirectional byte
	// copy; "http" terminates HTTP on the socket and reverse-proxies
	// each request to Target instead.
	Mode string `yaml:"mode" json:"mode"`
	// RewriteHost only applies to mode: http (rejected on any other
	// mode). false (the default) forwards the request to Target with
	// whatever Host header the client sent unchanged. true rewrites it
	// to Target's own host:port instead -- needed for a target that
	// routes or validates by hostname (tailscale serve, notably).
	RewriteHost bool `yaml:"rewrite_host" json:"rewrite_host"`
	// Enabled is a *bool (rather than bool) so "omitted" (nil, defaults
	// to true) is distinguishable from "explicitly false" -- a bridge
	// entry with enabled: false is known (visible via GET /bridges,
	// still occupying its name/listen path) but doesn't run: no socket
	// is created for it. This is normalizeBridge's job to resolve to a
	// concrete, always-non-nil pointer before the value is used or
	// re-persisted; see bridgeManager's Disable/Enable in manage.go for
	// how a bridge moves between the two states at runtime.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// Config is the fully resolved, validated configuration used at runtime.
type Config struct {
	Hostname              string
	StateDir              string
	Ephemeral             bool
	ControlURL            string
	SocketGroup           string
	SocketMode            os.FileMode
	ManagementSocket      string
	ManagementSocketGroup string
	ManagementSocketMode  os.FileMode
	Bridges               []BridgeConfig
}

// rawConfig mirrors the config.yaml schema.
type rawConfig struct {
	Hostname              string         `yaml:"hostname"`
	StateDir              string         `yaml:"state_dir"`
	Ephemeral             *bool          `yaml:"ephemeral"`
	ControlURL            string         `yaml:"control_url"`
	SocketGroup           string         `yaml:"socket_group"`
	SocketMode            string         `yaml:"socket_mode"`
	ManagementSocket      string         `yaml:"management_socket"`
	ManagementSocketGroup string         `yaml:"management_socket_group"`
	ManagementSocketMode  string         `yaml:"management_socket_mode"`
	Bridges               []BridgeConfig `yaml:"bridges"`
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
		normalizeBridge(&bridges[i])
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

	stateDir := resolvePath(baseDir, raw.StateDir)

	managementSocket := resolvePath(baseDir, raw.ManagementSocket)
	managementMode := defaultManagementSocketMode
	if managementSocket != "" {
		if raw.ManagementSocketMode != "" {
			managementMode, err = parseSocketMode(raw.ManagementSocketMode)
			if err != nil {
				return nil, fmt.Errorf("management_socket_mode: %w", err)
			}
		} else if raw.ManagementSocketGroup != "" {
			// The default mode is owner-only (0600), which grants the
			// group nothing -- so management_socket_group alone would
			// silently do nothing: the socket gets chowned to that
			// group, but its permission bits still don't let the group
			// use it. socket_group doesn't have this problem since its
			// own default (0660) already grants the group access.
			return nil, fmt.Errorf("management_socket_group is set but management_socket_mode is not: "+
				"the default %#o gives the group no access -- set a mode that does, e.g. \"0660\"", defaultManagementSocketMode)
		}
		if stateDir == "" {
			return nil, fmt.Errorf("management_socket requires state_dir to be set -- it's where managed bridges (%s) are persisted", managedBridgesFileName)
		}
		for _, b := range bridges {
			if b.Listen == managementSocket {
				return nil, fmt.Errorf("management_socket %q collides with bridge %q's listen path", managementSocket, b.Name)
			}
		}
	} else if raw.ManagementSocketMode != "" {
		return nil, fmt.Errorf("management_socket_mode is set but management_socket is empty")
	} else if raw.ManagementSocketGroup != "" {
		return nil, fmt.Errorf("management_socket_group is set but management_socket is empty")
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
		Hostname:              hostname,
		StateDir:              stateDir,
		Ephemeral:             ephemeral,
		ControlURL:            raw.ControlURL,
		SocketGroup:           raw.SocketGroup,
		SocketMode:            mode,
		ManagementSocket:      managementSocket,
		ManagementSocketGroup: raw.ManagementSocketGroup,
		ManagementSocketMode:  managementMode,
		Bridges:               bridges,
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

// validateBridgeFields checks one bridge's own fields in isolation --
// required fields present, mode supported, rewrite_host only where it
// applies. It doesn't know about any other bridge, so duplicate name/listen
// checks live in checkBridges (the static list) and bridgeManager.Add (a
// single new bridge against whatever's currently running) instead.
func validateBridgeFields(b BridgeConfig) error {
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
	return nil
}

// checkBridges validates required fields and rejects duplicate names or
// listen paths across the whole list.
func checkBridges(bridges []BridgeConfig) error {
	names := map[string]bool{}
	listens := map[string]bool{}
	for _, b := range bridges {
		if err := validateBridgeFields(b); err != nil {
			return err
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

// normalizeBridge lowercases/trims Mode (filling in the default when
// empty) and resolves Enabled to a concrete, always-non-nil pointer
// (defaulting to true when omitted). Applied uniformly to a bridge
// however it was sourced -- config.yaml, managed-bridges.yaml, or a
// management API request -- so all three accept the same shorthand
// (mode:/enabled: omitted) and end up with the same normalized shape.
func normalizeBridge(b *BridgeConfig) {
	b.Mode = strings.ToLower(strings.TrimSpace(b.Mode))
	if b.Mode == "" {
		b.Mode = defaultBridgeMode
	}
	if b.Enabled == nil {
		enabled := true
		b.Enabled = &enabled
	}
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
