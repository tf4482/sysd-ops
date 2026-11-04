// Shared configuration frame: lookup, first-run template, strict validation helpers.
// Copy unchanged; the app supplies defaultConfig, Config, and parseConfig.
package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	configFilename = "config.yml"
	placeholder    = "CHANGE_ME"
)

// Signal for a freshly written template
type configCreatedError struct{ path string }

func (e *configCreatedError) Error() string {
	return "created configuration template at " + e.path
}

// Directory of the running binary, symlinks resolved
func executableDir() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return filepath.Dir(executable), nil
}

// First existing file wins; no merging
func loadConfig(binaryDir, home string) (string, Config, error) {
	userPath := filepath.Join(home, ".config", projectName, configFilename)
	selected := ""
	for _, candidate := range []string{filepath.Join(binaryDir, configFilename), userPath} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			selected = candidate
			break
		}
	}
	if selected == "" {
		if err := createDefaultConfig(userPath); err != nil {
			return "", Config{}, err
		}
		return "", Config{}, &configCreatedError{userPath}
	}

	data, err := os.ReadFile(selected)
	if err != nil {
		return "", Config{}, fmt.Errorf("cannot read configuration %s: %v", selected, err)
	}
	var document any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return "", Config{}, fmt.Errorf("invalid YAML in %s%s", selected, yamlLocation(err))
	}
	config, err := parseConfig(document)
	return selected, config, err
}

var yamlLine = regexp.MustCompile(`line (\d+)`)

// Line number only; never echoes file content
func yamlLocation(err error) string {
	if match := yamlLine.FindStringSubmatch(err.Error()); match != nil {
		return " at line " + match[1]
	}
	return ""
}

// User-only template with dummy values
func createDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cannot create configuration %s: %v", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("configuration appeared while creating it: %s", path)
	}
	if err != nil {
		return fmt.Errorf("cannot create configuration %s: %v", path, err)
	}
	_, err = file.WriteString(defaultConfig)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cannot create configuration %s: %v", path, err)
	}
	return nil
}

// Validation helper keeping the first error
type reader struct{ err error }

func (r *reader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

func (r *reader) mapping(value any, location string) map[string]any {
	result, ok := value.(map[string]any)
	if !ok {
		r.fail("%s must be a YAML mapping", location)
	}
	return result
}

// Reject missing and unknown keys
func (r *reader) exactKeys(values map[string]any, location string, expected ...string) {
	known := map[string]bool{}
	var missing, unknown []string
	for _, key := range expected {
		known[key] = true
		if _, ok := values[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range values {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	var details []string
	if missing != nil {
		details = append(details, "missing: "+strings.Join(missing, ", "))
	}
	if unknown != nil {
		details = append(details, "unknown: "+strings.Join(unknown, ", "))
	}
	if details != nil {
		r.fail("%s keys are invalid (%s)", location, strings.Join(details, "; "))
	}
}

// Trimmed non-empty string without placeholder
func (r *reader) text(values map[string]any, key, location string) string {
	value, ok := values[key].(string)
	text := strings.TrimSpace(value)
	if !ok || text == "" {
		r.fail("%s.%s must be a non-empty string", location, key)
	} else if text == placeholder {
		r.fail("edit placeholder value at %s.%s", location, key)
	}
	return text
}

func (r *reader) port(values map[string]any, key, location string) int {
	value, ok := values[key].(int)
	if !ok || value < 1 || value > 65535 {
		r.fail("%s.%s must be an integer from 1 to 65535", location, key)
	}
	return value
}

func (r *reader) positiveInt(values map[string]any, key, location string) int {
	value, ok := values[key].(int)
	if !ok || value <= 0 {
		r.fail("%s.%s must be a positive integer", location, key)
	}
	return value
}

// Positive finite seconds as duration
func (r *reader) positiveSeconds(values map[string]any, key, location string) time.Duration {
	var seconds float64
	switch value := values[key].(type) {
	case int:
		seconds = float64(value)
	case float64:
		seconds = value
	}
	if !(seconds > 0) || math.IsInf(seconds, 0) {
		r.fail("%s.%s must be a positive number", location, key)
		return 0
	}
	// Clamp to avoid duration overflow
	return time.Duration(math.Min(seconds, 86400) * float64(time.Second))
}
