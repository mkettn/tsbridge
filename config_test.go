package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig_FullConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
hostname: bridge-test
state_dir: /var/lib/tsbridge
ephemeral: true
control_url: https://headscale.example.com
socket_group: www-data
socket_mode: "0640"
bridges:
  - name: svc-a
    listen: /run/tsbridge/svc-a.sock
    target: host-a:1111
    mode: TCP
  - name: svc-b
    listen: /run/tsbridge/svc-b.sock
    target: host-b:2222
`)

	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Hostname != "bridge-test" || !cfg.Ephemeral || cfg.SocketGroup != "www-data" || cfg.SocketMode != 0640 {
		t.Fatalf("unexpected top-level config: %+v", cfg)
	}
	if cfg.ControlURL != "https://headscale.example.com" {
		t.Errorf("want control_url %q, got %q", "https://headscale.example.com", cfg.ControlURL)
	}
	if len(cfg.Bridges) != 2 {
		t.Fatalf("want 2 bridges, got %d: %+v", len(cfg.Bridges), cfg.Bridges)
	}

	byName := map[string]BridgeConfig{}
	for _, b := range cfg.Bridges {
		byName[b.Name] = b
	}
	if byName["svc-a"].Target != "host-a:1111" {
		t.Errorf("svc-a not resolved correctly: %+v", byName["svc-a"])
	}
	if byName["svc-a"].Mode != "tcp" {
		t.Errorf("svc-a mode: want lowercased %q, got %q", "tcp", byName["svc-a"].Mode)
	}
	if byName["svc-b"].Target != "host-b:2222" {
		t.Errorf("svc-b not resolved correctly: %+v", byName["svc-b"])
	}
	if byName["svc-b"].Mode != "tcp" {
		t.Errorf("svc-b mode: want default %q, got %q", "tcp", byName["svc-b"].Mode)
	}
}

func TestLoadConfig_UnsupportedBridgeModeRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges:
  - name: svc
    listen: /run/svc.sock
    target: h:1
    mode: udp
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), `unsupported mode "udp"`) {
		t.Fatalf("want error naming unsupported mode, got: %v", err)
	}
}

func TestLoadConfig_HTTPModeAccepted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges:
  - name: svc
    listen: /run/svc.sock
    target: h:1
    mode: HTTP
`)
	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Bridges[0].Mode != "http" {
		t.Errorf("want lowercased %q, got %q", "http", cfg.Bridges[0].Mode)
	}
}

func TestLoadConfig_RelativePathsResolveAgainstConfigDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
state_dir: state
bridges:
  - name: relative-svc
    listen: relative.sock
    target: h:1
  - name: absolute-svc
    listen: /run/tsbridge/absolute.sock
    target: h:2
`)

	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	wantStateDir := filepath.Join(dir, "state")
	if cfg.StateDir != wantStateDir {
		t.Errorf("want state_dir %q, got %q", wantStateDir, cfg.StateDir)
	}

	byName := map[string]string{}
	for _, b := range cfg.Bridges {
		byName[b.Name] = b.Listen
	}
	if want := filepath.Join(dir, "relative.sock"); byName["relative-svc"] != want {
		t.Errorf("relative-svc listen: want %q, got %q", want, byName["relative-svc"])
	}
	if byName["absolute-svc"] != "/run/tsbridge/absolute.sock" {
		t.Errorf("absolute-svc listen: want unchanged, got %q", byName["absolute-svc"])
	}
}

func TestLoadConfig_DuplicateName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges:
  - name: dup
    listen: /run/a.sock
    target: h:1
  - name: dup
    listen: /run/b.sock
    target: h:2
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "duplicate bridge name") {
		t.Fatalf("expected duplicate name error, got: %v", err)
	}
}

func TestLoadConfig_DuplicateListen(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges:
  - name: a
    listen: /run/shared.sock
    target: h:1
  - name: b
    listen: /run/shared.sock
    target: h:2
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "duplicate listen path") {
		t.Fatalf("expected duplicate listen error, got: %v", err)
	}
}

func TestLoadConfig_MissingRequiredFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges:
  - name: no-target
    listen: /run/a.sock
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("expected missing-target error, got: %v", err)
	}
}

func TestLoadConfig_MalformedYAMLIsFatal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges: [this is not valid: yaml: [[[
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

func TestLoadConfig_UnknownTopLevelFieldRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges: []
totally_unknown_field: true
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatal("expected error for unknown top-level field, got nil")
	}
}

func TestLoadConfig_ControlURLRejectsNonHTTPS(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
control_url: http://headscale.example.com
bridges: []
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want error requiring https, got: %v", err)
	}
}

func TestLoadConfig_ControlURLRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
control_url: "headscale.example.com"
bridges: []
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "control_url") {
		t.Fatalf("want error naming control_url for a schemeless value, got: %v", err)
	}
}

func TestLoadConfig_ControlURLDefaultsToEmpty(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges: []
`)
	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ControlURL != "" {
		t.Errorf("want empty control_url (tsnet default) when unset, got %q", cfg.ControlURL)
	}
}

func TestLoadConfig_DefaultsWhenUnset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
bridges: []
`)
	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Hostname != "tsbridge" {
		t.Errorf("want default hostname %q, got %q", "tsbridge", cfg.Hostname)
	}
	if cfg.Ephemeral {
		t.Errorf("want ephemeral to default to false, got true")
	}
	if cfg.SocketMode != defaultSocketMode {
		t.Errorf("want default socket mode %v, got %v", defaultSocketMode, cfg.SocketMode)
	}
}
