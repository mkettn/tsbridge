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

func TestLoadConfig_InlineAndIncludeMerge(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
hostname: bridge-test
state_dir: /var/lib/tsbridge
ephemeral: true
control_url: https://headscale.example.com
socket_group: www-data
socket_mode: "0640"
include: config.d/*.yml
bridges:
  - name: inline-svc
    listen: /run/tsbridge/inline-svc.sock
    target: inline-host:1111
`)
	writeFile(t, filepath.Join(dir, "config.d", "a.yml"), `
bridges:
  - name: included-svc
    listen: /run/tsbridge/included-svc.sock
    target: included-host:2222
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

	byName := map[string]ResolvedBridge{}
	for _, b := range cfg.Bridges {
		byName[b.Name] = b
	}

	inline, ok := byName["inline-svc"]
	if !ok || inline.Source != "inline" || inline.Target != "inline-host:1111" {
		t.Errorf("inline-svc not resolved correctly: %+v", inline)
	}
	included, ok := byName["included-svc"]
	if !ok || included.Source != filepath.Join(dir, "config.d", "a.yml") || included.Target != "included-host:2222" {
		t.Errorf("included-svc not resolved correctly: %+v", included)
	}
}

func TestLoadConfig_EmptyGlobIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
hostname: bridge-test
include: config.d/*.yml
bridges:
  - name: only-svc
    listen: /run/tsbridge/only-svc.sock
    target: host:1
`)
	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Bridges) != 1 {
		t.Fatalf("want 1 bridge, got %d", len(cfg.Bridges))
	}
}

func TestLoadConfig_IncludeAsList(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
hostname: bridge-test
include:
  - config.d/one/*.yml
  - config.d/two/*.yml
bridges: []
`)
	writeFile(t, filepath.Join(dir, "config.d/one/a.yml"), `
bridges:
  - {name: a, listen: /run/a.sock, target: h:1}
`)
	writeFile(t, filepath.Join(dir, "config.d/two/b.yml"), `
bridges:
  - {name: b, listen: /run/b.sock, target: h:2}
`)
	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Bridges) != 2 {
		t.Fatalf("want 2 bridges, got %d: %+v", len(cfg.Bridges), cfg.Bridges)
	}
}

func TestLoadConfig_MalformedIncludedYAMLIsFatal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: config.d/*.yml
bridges: []
`)
	writeFile(t, filepath.Join(dir, "config.d", "broken.yml"), `
bridges: [this is not valid: yaml: [[[
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatal("expected error for malformed included YAML, got nil")
	}
	if !strings.Contains(err.Error(), "broken.yml") {
		t.Errorf("error should name the offending file, got: %v", err)
	}
}

func TestLoadConfig_DuplicateNameAcrossInlineAndIncluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: config.d/*.yml
bridges:
  - name: dup
    listen: /run/inline.sock
    target: h:1
`)
	writeFile(t, filepath.Join(dir, "config.d", "a.yml"), `
bridges:
  - name: dup
    listen: /run/included.sock
    target: h:2
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "duplicate bridge name") {
		t.Fatalf("expected duplicate name error, got: %v", err)
	}
}

func TestLoadConfig_DuplicateListenAcrossIncludedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: config.d/*.yml
bridges: []
`)
	writeFile(t, filepath.Join(dir, "config.d", "a.yml"), `
bridges:
  - {name: a, listen: /run/shared.sock, target: h:1}
`)
	writeFile(t, filepath.Join(dir, "config.d", "b.yml"), `
bridges:
  - {name: b, listen: /run/shared.sock, target: h:2}
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "duplicate listen path") {
		t.Fatalf("expected duplicate listen error, got: %v", err)
	}
}

func TestLoadConfig_NestedIncludeRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: config.d/*.yml
bridges: []
`)
	writeFile(t, filepath.Join(dir, "config.d", "a.yml"), `
include: config.d/nested/*.yml
bridges:
  - {name: a, listen: /run/a.sock, target: h:1}
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "cannot themselves include") {
		t.Fatalf("expected nested-include rejection, got: %v", err)
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

func TestLoadConfig_IncludedFileGlobalOptionRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: config.d/*.yml
bridges: []
`)
	writeFile(t, filepath.Join(dir, "config.d", "a.yml"), `
socket_mode: "0600"
bridges:
  - {name: a, listen: /run/a.sock, target: h:1}
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "socket_mode") {
		t.Fatalf("expected error naming socket_mode as a rejected global option, got: %v", err)
	}
}

func TestLoadConfig_ControlURLRejectedFromIncludedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: config.d/*.yml
bridges: []
`)
	writeFile(t, filepath.Join(dir, "config.d", "a.yml"), `
control_url: https://headscale.example.com
bridges:
  - {name: a, listen: /run/a.sock, target: h:1}
`)
	_, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "control_url") {
		t.Fatalf("expected error naming control_url as a rejected global option, got: %v", err)
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

func TestLoadConfig_OverlappingGlobsDeduped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), `
include:
  - config.d/*.yml
  - config.d/dup-*.yml
bridges: []
`)
	writeFile(t, filepath.Join(dir, "config.d", "dup-svc.yml"), `
bridges:
  - {name: dup-svc, listen: /run/dup-svc.sock, target: h:1}
`)
	cfg, err := LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v (overlapping globs should be deduped, not treated as a conflict)", err)
	}
	if len(cfg.Bridges) != 1 {
		t.Fatalf("want 1 bridge (file matched by both globs counted once), got %d: %+v", len(cfg.Bridges), cfg.Bridges)
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
