//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Grace period between SIGTERM and SIGKILL for child processes
const stopDelay = 500 * time.Millisecond

// Fixed properties read with systemctl show, one block per unit in argument order
var unitProperties = []string{
	"LoadState", "ActiveState", "SubState", "UnitFileState", "FragmentPath", "DropInPaths",
	"NextElapseUSecRealtime", "LastTriggerUSec", "Result",
}

// State of one unit as reported by systemctl show
type Unit struct {
	Name          string `json:"name"`
	LoadState     string `json:"load_state"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	UnitFileState string `json:"unit_file_state"`
	FragmentPath  string `json:"fragment_path"`
	DropInPaths   string `json:"drop_in_paths,omitempty"`
	NextElapse    string `json:"next_elapse,omitempty"`
	LastTrigger   string `json:"last_trigger,omitempty"`
	Result        string `json:"result,omitempty"`
}

// Unit exists when systemd loaded it
func (u *Unit) exists() bool {
	return u != nil && u.LoadState == "loaded"
}

// Captured run with deadline and C locale; returns stdout
func (s *session) query(ctx context.Context, name string, arguments ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.config.CommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = stopDelay
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), commandError(name, arguments, err, stderr.String())
	}
	return stdout.String(), nil
}

// Run on the terminal without deadline, for editors and sudo password prompts
func (s *session) attached(ctx context.Context, environment []string, name string, arguments ...string) error {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Env = append(os.Environ(), environment...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = stopDelay
	if err := command.Run(); err != nil {
		return commandError(name, arguments, err, "")
	}
	return nil
}

// Short, single-line reason for a failed program run
func commandError(name string, arguments []string, err error, stderr string) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("missing local dependency: %s", name)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: timeout", commandText(name, arguments))
	}
	reason := strings.Join(strings.Fields(stderr), " ")
	if reason == "" {
		reason = err.Error()
	}
	return fmt.Errorf("%s failed: %s", commandText(name, arguments), reason)
}

// Command line for messages and dry runs
func commandText(name string, arguments []string) string {
	return strings.TrimSpace(name + " " + strings.Join(arguments, " "))
}

// Changing systemctl call: through sudo in system scope, only printed in a dry run
func (s *session) systemctl(ctx context.Context, scope string, arguments ...string) error {
	if scope == scopeSystem {
		return s.privileged(ctx, append([]string{"systemctl"}, arguments...)...)
	}
	arguments = append([]string{"--user"}, arguments...)
	if s.dryRun {
		s.out.line("⏭️", "Would run: "+commandText("systemctl", arguments), yellow)
		return nil
	}
	_, err := s.query(ctx, "systemctl", arguments...)
	return err
}

// Fixed command through sudo, attached for the password prompt; only printed in a dry run
func (s *session) privileged(ctx context.Context, arguments ...string) error {
	if s.dryRun {
		s.out.line("⏭️", "Would run: "+commandText("sudo", arguments), yellow)
		return nil
	}
	return s.attached(ctx, nil, "sudo", arguments...)
}

// systemctl prefix arguments of a scope
func scopeArguments(scope string) []string {
	if scope == scopeUser {
		return []string{"--user"}
	}
	return nil
}

// Systemd must run and systemctl must exist
func systemdAvailable() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("missing local dependency: systemctl")
	}
	if info, err := os.Stat("/run/systemd/system"); err != nil || !info.IsDir() {
		return errors.New("systemd is not running on this host")
	}
	return nil
}

// Read the fixed properties of units in one scope, in the given order
func (s *session) showUnits(ctx context.Context, scope string, names []string) ([]Unit, error) {
	if len(names) == 0 {
		return nil, nil
	}
	arguments := append(scopeArguments(scope), "show", "--property="+strings.Join(unitProperties, ","), "--")
	output, err := s.query(ctx, "systemctl", append(arguments, names...)...)
	if err != nil {
		return nil, err
	}
	blocks, err := parseShow(output)
	if err != nil {
		return nil, err
	}
	if len(blocks) != len(names) {
		return nil, fmt.Errorf("systemctl show returned %d units for %d names", len(blocks), len(names))
	}
	units := make([]Unit, len(names))
	for index, block := range blocks {
		units[index] = Unit{
			Name:          names[index],
			LoadState:     block["LoadState"],
			ActiveState:   block["ActiveState"],
			SubState:      block["SubState"],
			UnitFileState: block["UnitFileState"],
			FragmentPath:  block["FragmentPath"],
			DropInPaths:   block["DropInPaths"],
			NextElapse:    block["NextElapseUSecRealtime"],
			LastTrigger:   block["LastTriggerUSec"],
			Result:        block["Result"],
		}
	}
	return units, nil
}

// KEY=VALUE blocks separated by blank lines; the value keeps further = signs
func parseShow(output string) ([]map[string]string, error) {
	var blocks []map[string]string
	var block map[string]string
	for number, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			block = nil
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("unexpected systemctl show output at line %d", number+1)
		}
		if block == nil {
			block = map[string]string{}
			blocks = append(blocks, block)
		}
		block[key] = value
	}
	return blocks, nil
}
