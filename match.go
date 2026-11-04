//go:build linux

package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// States of a script and its units in one scope
const (
	stateScriptOnly  = "script-only"
	stateComplete    = "complete"
	stateServiceOnly = "service-only"
	stateTimerOnly   = "timer-only"
)

// Actions offered per state
const (
	actionInstall     = "Install"
	actionEditTimer   = "Edit timer"
	actionEditService = "Edit service"
	actionUninstall   = "Uninstall timer"
)

// Characters systemd allows in unit names
var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.\-]+$`)

// One table row: a script and its units in one scope
type Entry struct {
	Script  Script `json:"script"`
	Scope   string `json:"scope,omitempty"`
	State   string `json:"state"`
	Timer   *Unit  `json:"timer,omitempty"`
	Service *Unit  `json:"service,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// Reason a script name cannot name a unit; empty when valid
func nameProblem(name string) string {
	switch {
	case !unitName.MatchString(name):
		return "not installable: name may only contain A-Z a-z 0-9 : _ . -"
	case strings.HasPrefix(name, "-"), strings.HasPrefix(name, "."), strings.Contains(name, ".."):
		return "not installable: name must not start with - or . and must not contain .."
	}
	return ""
}

// Scripts with their units; units maps scope to unit name to unit
func buildEntries(scripts []Script, units map[string]map[string]Unit, scopes []string) []Entry {
	count := map[string]int{}
	for _, script := range scripts {
		count[script.Name]++
	}
	var entries []Entry
	for _, script := range scripts {
		problem := nameProblem(script.Name)
		if problem == "" && count[script.Name] > 1 {
			problem = fmt.Sprintf("ambiguous: %d scripts share the name %s", count[script.Name], script.Name)
		}
		found := false
		for _, scope := range scopes {
			timer, service := lookupUnit(units, scope, script.Name+".timer"), lookupUnit(units, scope, script.Name+".service")
			if !timer.exists() && !service.exists() {
				continue
			}
			found = true
			entries = append(entries, Entry{script, scope, unitState(timer, service), timer, service, problem})
		}
		if !found {
			entries = append(entries, Entry{Script: script, State: stateScriptOnly, Problem: problem})
		}
	}
	return entries
}

// Existing unit of a scope by exact name, nil otherwise
func lookupUnit(units map[string]map[string]Unit, scope, name string) *Unit {
	unit, ok := units[scope][name]
	if !ok || !unit.exists() {
		return nil
	}
	return &unit
}

// State from the existence of timer and service
func unitState(timer, service *Unit) string {
	switch {
	case timer.exists() && service.exists():
		return stateComplete
	case service.exists():
		return stateServiceOnly
	case timer.exists():
		return stateTimerOnly
	}
	return stateScriptOnly
}

// Actions that fit the state; none for problem rows
func (e Entry) actions() []string {
	if e.Problem != "" {
		return nil
	}
	switch e.State {
	case stateComplete:
		return []string{actionEditTimer, actionEditService, actionUninstall}
	case stateServiceOnly:
		return []string{actionInstall, actionEditService}
	case stateTimerOnly:
		return []string{actionInstall, actionEditTimer, actionUninstall}
	}
	return []string{actionInstall}
}

// Scan the directory and query systemd for every valid script name
func (s *session) readEntries(ctx context.Context) ([]Script, []Entry, error) {
	scripts, err := findScripts(s.directory, s.recursive)
	if err != nil {
		return nil, nil, err
	}
	if err := systemdAvailable(); err != nil {
		return scripts, nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, script := range scripts {
		if nameProblem(script.Name) == "" && !seen[script.Name] {
			seen[script.Name] = true
			names = append(names, script.Name+".timer", script.Name+".service")
		}
	}
	units := map[string]map[string]Unit{}
	for _, scope := range s.scopes {
		found, err := s.showUnits(ctx, scope, names)
		if err != nil {
			return scripts, nil, fmt.Errorf("%s scope: %v", scope, err)
		}
		units[scope] = map[string]Unit{}
		for _, unit := range found {
			units[scope][unit.Name] = unit
		}
	}
	return scripts, buildEntries(scripts, units, s.scopes), nil
}
