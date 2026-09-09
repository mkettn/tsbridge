package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultSocketMode os.FileMode = 0660

// BridgeConfig is one listen-socket -> tailnet-target mapping, as it
// appears in YAML (inline or in an included file).
type BridgeConfig struct {
	Name   string `yaml:"name"`
	Listen string `yaml:"listen"`
	Target string `yaml:"target"`
}

// ResolvedBridge is a BridgeConfig annotated with where it came from, for
// startup logging and duplicate-detection error messages.
type ResolvedBridge struct {
	BridgeConfig
	Source string // "inline" or the path of the included file
}

// Config is the fully resolved, validated configuration used at runtime.
type Config struct {
	Hostname    string
	StateDir    string
	Ephemeral   bool
	ControlURL  string
	SocketGroup string
	SocketMode  os.FileMode
	Bridges     []ResolvedBridge
}

// stringList unmarshals an `include:` value that may be a single glob
// string or a list of globs.
type stringList []string

func (s *stringList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var single string
		if err := value.Decode(&single); err != nil {
			return err
		}
		if single == "" {
			*s = nil
			return nil
		}
		*s = stringList{single}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*s = stringList(list)
		return nil
	default:
		return fmt.Errorf("must be a string or a list of strings")
	}
}

// rawConfig mirrors the top-level YAML schema. Both the main config file
// and included files decode into it; included files are only permitted to
// set Bridges (Include is checked and rejected separately).
type rawConfig struct {
	Hostname    string         `yaml:"hostname"`
	StateDir    string         `yaml:"state_dir"`
	Ephemeral   *bool          `yaml:"ephemeral"`
	ControlURL  string         `yaml:"control_url"`
	SocketGroup string         `yaml:"socket_group"`
	SocketMode  string         `yaml:"socket_mode"`
	Include     stringList     `yaml:"include"`
	Bridges     []BridgeConfig `yaml:"bridges"`
}

// LoadConfig reads the main config file at path, merges in any files
// matched by its include globs, validates the result, and returns the
// fully resolved configuration. Any error returned here is fatal: callers
// should log it and exit non-zero.
func LoadConfig(path string) (*Config, error) {
	main, err := readRawConfig(path)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}

	resolved := make([]ResolvedBridge, 0, len(main.Bridges))
	for _, b := range main.Bridges {
		resolved = append(resolved, ResolvedBridge{BridgeConfig: b, Source: "inline"})
	}

	baseDir := filepath.Dir(path)
	includeFiles, err := expandIncludes(baseDir, main.Include)
	if err != nil {
		return nil, err
	}

	for _, f := range includeFiles {
		inc, err := readRawConfig(f)
		if err != nil {
			return nil, fmt.Errorf("loading included file %s: %w", f, err)
		}
		if len(inc.Include) > 0 {
			return nil, fmt.Errorf("included file %s sets 'include', but included files cannot themselves include other files", f)
		}
		if err := rejectGlobalOptions(f, inc); err != nil {
			return nil, err
		}
		for _, b := range inc.Bridges {
			resolved = append(resolved, ResolvedBridge{BridgeConfig: b, Source: f})
		}
	}

	if err := checkBridges(resolved); err != nil {
		return nil, err
	}

	mode := defaultSocketMode
	if main.SocketMode != "" {
		mode, err = parseSocketMode(main.SocketMode)
		if err != nil {
			return nil, fmt.Errorf("socket_mode: %w", err)
		}
	}

	ephemeral := false
	if main.Ephemeral != nil {
		ephemeral = *main.Ephemeral
	}

	hostname := main.Hostname
	if hostname == "" {
		hostname = "tsbridge"
	}

	return &Config{
		Hostname:    hostname,
		StateDir:    main.StateDir,
		Ephemeral:   ephemeral,
		ControlURL:  main.ControlURL,
		SocketGroup: main.SocketGroup,
		SocketMode:  mode,
		Bridges:     resolved,
	}, nil
}

// rejectGlobalOptions errors out if an included file sets any top-level
// option other than bridges: an included file that sets, say,
// socket_mode has that value silently dropped (only Bridges is ever
// consumed from it), which for a value like socket_mode is a silent
// permission downgrade relative to what the operator wrote. Failing
// fast here is the same class of guard as the nested-include check.
func rejectGlobalOptions(file string, inc *rawConfig) error {
	var set []string
	if inc.Hostname != "" {
		set = append(set, "hostname")
	}
	if inc.StateDir != "" {
		set = append(set, "state_dir")
	}
	if inc.Ephemeral != nil {
		set = append(set, "ephemeral")
	}
	if inc.ControlURL != "" {
		set = append(set, "control_url")
	}
	if inc.SocketGroup != "" {
		set = append(set, "socket_group")
	}
	if inc.SocketMode != "" {
		set = append(set, "socket_mode")
	}
	if len(set) > 0 {
		return fmt.Errorf("included file %s sets global option(s) %s, but included files may only set 'bridges'", file, strings.Join(set, ", "))
	}
	return nil
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

// expandIncludes resolves each include glob (relative globs are resolved
// against the main config file's directory) and returns the matched files
// in sorted order per glob, globs concatenated in the order given. A file
// reachable through more than one glob (overlapping patterns) is only
// loaded once. A glob matching zero files is not an error.
func expandIncludes(baseDir string, patterns stringList) ([]string, error) {
	var files []string
	seen := map[string]bool{}
	for _, pattern := range patterns {
		resolved := pattern
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(baseDir, resolved)
		}
		matches, err := filepath.Glob(resolved)
		if err != nil {
			return nil, fmt.Errorf("invalid include glob %q: %w", pattern, err)
		}
		sort.Strings(matches)
		for _, m := range matches {
			abs, err := filepath.Abs(m)
			if err != nil {
				return nil, fmt.Errorf("resolving %s: %w", m, err)
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			files = append(files, m)
		}
	}
	return files, nil
}

// checkBridges validates required fields and rejects duplicate names or
// listen paths across inline and included bridges.
func checkBridges(bridges []ResolvedBridge) error {
	names := map[string]string{}
	listens := map[string]string{}
	for _, b := range bridges {
		if b.Name == "" {
			return fmt.Errorf("bridge in %s is missing required field 'name'", b.Source)
		}
		if b.Listen == "" {
			return fmt.Errorf("bridge %q in %s is missing required field 'listen'", b.Name, b.Source)
		}
		if b.Target == "" {
			return fmt.Errorf("bridge %q in %s is missing required field 'target'", b.Name, b.Source)
		}
		if prev, ok := names[b.Name]; ok {
			return fmt.Errorf("duplicate bridge name %q: defined in both %s and %s", b.Name, prev, b.Source)
		}
		names[b.Name] = b.Source
		if prev, ok := listens[b.Listen]; ok {
			return fmt.Errorf("duplicate listen path %q: defined in both %s and %s", b.Listen, prev, b.Source)
		}
		listens[b.Listen] = b.Source
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
