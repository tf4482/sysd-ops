//go:build linux

package main

import (
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Scope names as used by systemd and in the output
const (
	scopeUser   = "user"
	scopeSystem = "system"
	scopeAll    = "all"
)

// Must match config.example.yml
const defaultConfig = `settings:
  # Script directory; empty uses the working directory
  directory: ""
  # Search subdirectories too
  recursive: false
  # Editor for the edit actions
  editor: "nano"
  # Units shown: system, user, or all
  scope: "all"
  # Default scope when installing: user or system
  install_scope: "user"
  # Directory with service.tmpl and timer.tmpl; empty uses the default lookup
  template_dir: ""
  # Deadline for each systemctl, systemd-analyze, and loginctl call
  command_timeout_seconds: 30
`

// Validated configuration
type Config struct {
	Directory      string
	Recursive      bool
	Editor         string
	Scope          string
	InstallScope   string
	TemplateDir    string
	CommandTimeout time.Duration
}

// Strict validation of the decoded YAML document
func parseConfig(document any) (Config, error) {
	r := &reader{}
	root := r.mapping(document, "configuration")
	settings := r.mapping(root["settings"], "settings")
	r.exactKeys(root, "configuration", "settings")
	r.exactKeys(settings, "settings", "directory", "recursive", "editor", "scope",
		"install_scope", "template_dir", "command_timeout_seconds")

	config := Config{
		Directory:      optionalPath(r, settings, "directory"),
		Recursive:      flag(r, settings, "recursive"),
		Editor:         r.text(settings, "editor", "settings"),
		Scope:          choice(r, settings, "scope", scopeSystem, scopeUser, scopeAll),
		InstallScope:   choice(r, settings, "install_scope", scopeUser, scopeSystem),
		TemplateDir:    optionalPath(r, settings, "template_dir"),
		CommandTimeout: r.positiveSeconds(settings, "command_timeout_seconds", "settings"),
	}
	if r.err != nil {
		return Config{}, r.err
	}
	return config, nil
}

// Trimmed string that may be empty
func optionalPath(r *reader, values map[string]any, key string) string {
	value, ok := values[key].(string)
	if !ok {
		r.fail("settings.%s must be a string", key)
	}
	return strings.TrimSpace(value)
}

// YAML boolean
func flag(r *reader, values map[string]any, key string) bool {
	value, ok := values[key].(bool)
	if !ok {
		r.fail("settings.%s must be true or false", key)
	}
	return value
}

// One of a fixed set of words, matched case-insensitively
func choice(r *reader, values map[string]any, key string, allowed ...string) string {
	value, _ := values[key].(string)
	value = strings.ToLower(strings.TrimSpace(value))
	if !slices.Contains(allowed, value) {
		r.fail("settings.%s must be one of %s", key, strings.Join(allowed, ", "))
	}
	return value
}

// Absolute path; ~/ expands to home, relative paths start at base
func expandPath(path, home, base string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}
