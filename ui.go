//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Menu entries besides the state's actions
const (
	choiceBack = "Back"
	choiceQuit = "Quit"
)

// Marker errors checked with errors.Is
var (
	errAborted     = errors.New("aborted on request")
	errMissingTool = errors.New("missing local dependency")
)

// Per-run state shared by all actions
type session struct {
	out             printer
	config          Config
	directory       string
	directorySource string
	recursive       bool
	scopes          []string
	templates       map[string]unitTemplate
	userUnits       string
	systemUnits     string
	account         string
	dryRun          bool
	yes             bool
}

// Exit code of an action; a declined confirmation is no error
func (s *session) report(category string, err error) int {
	switch {
	case errors.Is(err, errAborted):
		s.out.line("⏭️", "Nothing changed", yellow)
		return 0
	case err != nil:
		return s.out.fail(category, err)
	}
	if s.dryRun {
		s.out.line("✅", "Dry run finished · nothing changed", green)
	}
	return 0
}

// Origin of each template for the start-up line
func (s *session) templateSources() string {
	return kindService + ".tmpl " + s.templates[kindService].source + " · " +
		kindTimer + ".tmpl " + s.templates[kindTimer].source
}

// Print the table or JSON; scripts alone when systemd cannot be queried
func (s *session) list(ctx context.Context, asJSON, verbose bool) int {
	scripts, entries, err := s.readEntries(ctx)
	if err != nil && scripts == nil {
		return s.out.fail("Scan", err)
	}
	if err != nil {
		for _, script := range scripts {
			s.out.line("📜", script.Name+" · "+script.Path, "")
		}
		return s.out.fail("Systemd", err)
	}
	if asJSON {
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return s.out.fail("Output", err)
		}
		s.out.print(string(data), "")
		return 0
	}
	if len(entries) == 0 {
		s.out.line("⚠️", "No shell scripts in "+s.directory, yellow)
		return 0
	}
	rows := alignRows(entryCells(entries))
	for index, entry := range entries {
		icon, color := stateLook(entry)
		s.out.line(icon, rows[index], color)
		if verbose {
			for _, unit := range []*Unit{entry.Timer, entry.Service} {
				if unit.exists() {
					s.out.print("     "+unit.Name+" · "+unit.FragmentPath+" · drop-ins "+orDash(unit.DropInPaths)+" · result "+orDash(unit.Result), "")
				}
			}
		}
	}
	if s.out.interactive || verbose {
		return 0
	}
	s.out.line("✅", fmt.Sprintf("%d scripts listed", len(scripts)), green)
	return 0
}

// Icon and color of a table row
func stateLook(entry Entry) (string, string) {
	switch {
	case entry.Problem != "":
		return "⚠️", yellow
	case entry.State == stateComplete:
		return "✅", green
	case entry.State == stateScriptOnly:
		return "📜", ""
	}
	return "⚠️", yellow
}

// Table cells: number, name, state, scope, timer, service, details
func entryCells(entries []Entry) [][]string {
	rows := make([][]string, len(entries))
	for index, entry := range entries {
		details := entry.Problem
		if details == "" && entry.Timer.exists() {
			details = "next " + orDash(entry.Timer.NextElapse) + " · last " + orDash(entry.Timer.LastTrigger)
		}
		rows[index] = []string{
			fmt.Sprintf("%d.", index+1), entry.Script.Name, entry.State, orDash(entry.Scope),
			unitCell("timer", entry.Timer), unitCell("service", entry.Service), details,
		}
	}
	return rows
}

// Active and enablement state of a unit, or its absence
func unitCell(kind string, unit *Unit) string {
	if !unit.exists() {
		return "no " + kind
	}
	return kind + " " + unit.ActiveState + "/" + orDash(unit.UnitFileState)
}

// Pad every column to its widest cell; emojis count two cells
func alignRows(rows [][]string) []string {
	var widths []int
	for _, row := range rows {
		for column, cell := range row {
			if column == len(widths) {
				widths = append(widths, 0)
			}
			widths[column] = max(widths[column], cellWidth(cell))
		}
	}
	lines := make([]string, len(rows))
	for index, row := range rows {
		var line strings.Builder
		for column, cell := range row {
			line.WriteString(cell + strings.Repeat(" ", widths[column]-cellWidth(cell)+2))
		}
		lines[index] = strings.TrimRight(line.String(), " ")
	}
	return lines
}

// Entry of a script name whose state allows the action; problems and several scopes are errors
func (s *session) findEntry(ctx context.Context, name string, allowed func(Entry) bool) (Entry, error) {
	_, entries, err := s.readEntries(ctx)
	if err != nil {
		return Entry{}, err
	}
	var matches []Entry
	known := false
	for _, entry := range entries {
		if entry.Script.Name != name {
			continue
		}
		known = true
		if entry.Problem != "" {
			return Entry{}, fmt.Errorf("%s: %s", name, entry.Problem)
		}
		if allowed(entry) {
			matches = append(matches, entry)
		}
	}
	switch {
	case !known:
		return Entry{}, fmt.Errorf("no script named %s in %s", name, s.directory)
	case len(matches) == 0:
		return Entry{}, fmt.Errorf("this action does not fit the state of %s", name)
	case len(matches) > 1:
		return Entry{}, fmt.Errorf("%s has units in several scopes; pass --user or --system", name)
	}
	return matches[0], nil
}

// Interactive loop: pick a script, pick an action, reread the list
func (s *session) browse(ctx context.Context) int {
	failed := false
	for ctx.Err() == nil {
		scripts, entries, err := s.readEntries(ctx)
		if err != nil {
			for _, script := range scripts {
				s.out.line("📜", script.Name+" · "+script.Path, "")
			}
			return s.out.fail("Systemd", err)
		}
		if len(entries) == 0 {
			s.out.line("⚠️", "No shell scripts in "+s.directory, yellow)
			return 0
		}
		rows := alignRows(entryCells(entries))
		index := 0
		if len(entries) > 1 {
			var ok bool
			if index, ok = s.out.choose(ctx, "🎯 Select a script", rows); !ok {
				break
			}
		}
		entry := entries[index]
		icon, color := stateLook(entry)
		s.out.line(icon, rows[index], color)
		if entry.Problem != "" {
			s.out.line("⚠️", entry.Script.Name+" · no actions · "+entry.Problem, yellow)
			if len(entries) == 1 {
				break
			}
			continue
		}
		choices := append(entry.actions(), choiceBack, choiceQuit)
		if len(entries) == 1 {
			choices = slices.DeleteFunc(choices, func(choice string) bool { return choice == choiceBack })
		}
		choice, ok := s.out.choose(ctx, "🛠️ "+entry.Script.Name+" · "+entry.State, choices)
		if !ok || choices[choice] == choiceQuit {
			break
		}
		if choices[choice] == choiceBack {
			continue
		}
		failed = s.perform(ctx, entry, choices[choice]) != 0 || failed
	}
	if failed {
		return 1
	}
	return 0
}

// Run one menu action and report its outcome
func (s *session) perform(ctx context.Context, entry Entry, action string) int {
	switch action {
	case actionInstall:
		plan, err := s.wizard(ctx, entry)
		if err == nil {
			err = s.install(ctx, plan)
		}
		if code := s.report("Install", err); err != nil || s.dryRun {
			return code
		}
		if ok, err := s.ask(ctx, "Open the new units in "+s.config.Editor+"?"); err != nil || !ok {
			return 0
		}
		for _, kind := range plan.Kinds {
			if err := s.edit(ctx, plan.Scope, entry.Script.Name+"."+kind, false); err != nil {
				return s.out.fail("Edit", err)
			}
		}
		return 0
	case actionEditTimer:
		return s.report("Edit", s.edit(ctx, entry.Scope, entry.Timer.Name, false))
	case actionEditService:
		return s.report("Edit", s.edit(ctx, entry.Scope, entry.Service.Name, false))
	}
	return s.report("Uninstall", s.uninstall(ctx, entry))
}

// Schedule presets of the wizard and the timer fields each needs
var presets = []struct {
	label  string
	fields []string
}{
	{"default · OnBootSec=60s, OnUnitInactiveSec=15s", []string{"OnBootSec", "OnUnitInactiveSec"}},
	{"hourly", []string{"OnCalendar"}},
	{"daily", []string{"OnCalendar"}},
	{"weekly", []string{"OnCalendar"}},
	{"monthly", []string{"OnCalendar"}},
	{"on boot · OnBootSec=", []string{"OnBootSec"}},
	{"interval after the previous run finished · OnUnitInactiveSec=", []string{"OnUnitInactiveSec"}},
	{"interval after the previous run started · OnUnitActiveSec=", []string{"OnUnitActiveSec"}},
	{"custom OnCalendar= expression", []string{"OnCalendar"}},
}

// Interactive install plan; asks only for fields the templates in use reference
func (s *session) wizard(ctx context.Context, entry Entry) (Plan, error) {
	scope := s.config.InstallScope
	if entry.Scope == "" {
		scopes := []string{scopeUser, scopeSystem}
		if scope == scopeSystem {
			scopes = []string{scopeSystem, scopeUser}
		}
		index, ok := s.out.choose(ctx, "🧭 Install scope", scopes)
		if !ok {
			return Plan{}, errAborted
		}
		scope = scopes[index]
		s.out.line("🧭", "Scope: "+scope, "")
	}
	plan, err := s.basePlan(entry, scope)
	if err != nil {
		return Plan{}, err
	}
	timer := &plan.Timer
	uses := func(kind, field string) bool {
		return slices.Contains(plan.Kinds, kind) && s.templates[kind].fields[field]
	}
	if slices.Contains(plan.Kinds, kindTimer) {
		if err := s.askSchedule(ctx, timer); err != nil {
			return Plan{}, err
		}
	}
	if timer.OnCalendar != "" && uses(kindTimer, "Persistent") {
		answer, err := s.prompt(ctx, "Catch up missed runs (Persistent=), yes or no", "yes", func(value string) error {
			if value != "yes" && value != "no" {
				return errors.New("answer yes or no")
			}
			return nil
		})
		if err != nil {
			return Plan{}, err
		}
		timer.Persistent = answer == "yes"
	}
	spans := []struct {
		field, question string
		value           *string
	}{
		{"AccuracySec", "Accuracy (AccuracySec=)", &timer.AccuracySec},
		{"RandomizedDelaySec", "Random delay (RandomizedDelaySec=)", &timer.RandomizedDelaySec},
	}
	for _, span := range spans {
		if uses(kindTimer, span.field) {
			if *span.value, err = s.prompt(ctx, span.question, *span.value, s.spanCheck(ctx)); err != nil {
				return Plan{}, err
			}
		}
	}
	for _, kind := range plan.Kinds {
		data := &plan.Service
		if kind == kindTimer {
			data = timer
		}
		if uses(kind, "Description") {
			description, err := s.prompt(ctx, "Description of the "+kind, unescapeWord(data.Description), checkLine)
			if err != nil {
				return Plan{}, err
			}
			data.Description = settingValue(description)
		}
	}
	if plan.Scope == scopeSystem && (uses(kindService, "User") || uses(kindTimer, "User")) {
		account, err := s.prompt(ctx, "Run as user (User=)", plan.Service.User, func(value string) error {
			if !accountName.MatchString(value) {
				return errors.New("invalid account name")
			}
			return nil
		})
		if err != nil {
			return Plan{}, err
		}
		plan.Service.User, timer.User = account, account
	}
	if uses(kindService, "WorkDir") || uses(kindTimer, "WorkDir") {
		directory, err := s.prompt(ctx, "Working directory", unescapeWord(plan.Service.WorkDir), checkLine)
		if err != nil {
			return Plan{}, err
		}
		plan.Service.WorkDir, timer.WorkDir = settingValue(directory), settingValue(directory)
	}
	return plan, nil
}

// Schedule preset and its values; presets need fields the timer template references
func (s *session) askSchedule(ctx context.Context, timer *RenderData) error {
	fields := s.templates[kindTimer].fields
	var labels []string
	for _, preset := range presets {
		if !slices.ContainsFunc(preset.fields, func(field string) bool { return !fields[field] }) {
			labels = append(labels, preset.label)
		}
	}
	if len(labels) == 0 {
		return nil
	}
	index, ok := s.out.choose(ctx, "⏰ Schedule", labels)
	if !ok {
		return errAborted
	}
	label := labels[index]
	s.out.line("⏰", "Schedule: "+label, "")
	name, _, _ := strings.Cut(label, " ")
	if label == presets[0].label {
		return nil
	}
	timer.OnBootSec, timer.OnUnitInactiveSec = "", ""
	var err error
	switch name {
	case "hourly", "monthly":
		timer.OnCalendar = name
	case "daily", "weekly":
		day := ""
		if name == "weekly" {
			if day, err = s.prompt(ctx, "Weekday (Mon to Sun)", "Mon", func(value string) error {
				if !validWeekday(value) {
					return errors.New("use Mon, Tue, Wed, Thu, Fri, Sat, or Sun")
				}
				return nil
			}); err != nil {
				return err
			}
			day = weekdayName(day) + " "
		}
		clock, err := s.prompt(ctx, "Time (HH:MM)", "03:00", func(value string) error {
			if !validClock(value) {
				return errors.New("use HH:MM, e.g. 03:00")
			}
			return nil
		})
		if err != nil {
			return err
		}
		timer.OnCalendar = day + "*-*-* " + clock + ":00"
	case "on":
		timer.OnBootSec, err = s.prompt(ctx, "Delay after boot", defaultOnBootSec, s.spanCheck(ctx))
		return err
	case "interval":
		field := &timer.OnUnitInactiveSec
		if strings.Contains(label, "started") {
			field = &timer.OnUnitActiveSec
		}
		if *field, err = s.prompt(ctx, "Interval", "15min", s.spanCheck(ctx)); err != nil {
			return err
		}
		// Intervals count from the last run, so the first run needs a boot delay
		timer.OnBootSec, err = s.prompt(ctx, "First run after boot", defaultOnBootSec, s.spanCheck(ctx))
		return err
	case "custom":
		timer.OnCalendar, err = s.prompt(ctx, "OnCalendar= expression", "*-*-* 03:00:00", func(value string) error {
			_, err := s.checkCalendar(ctx, value)
			return err
		})
		if err != nil {
			return err
		}
	}
	next, err := s.checkCalendar(ctx, timer.OnCalendar)
	if err != nil {
		return err
	}
	s.out.line("🗓️", "OnCalendar="+timer.OnCalendar+" · next elapse "+next, "")
	return nil
}

// Time span validation through systemd-analyze
func (s *session) spanCheck(ctx context.Context) func(string) error {
	return func(value string) error { return s.checkTimespan(ctx, value) }
}

// Ask until the check passes; empty input takes the default
func (s *session) prompt(ctx context.Context, question, fallback string, check func(string) error) (string, error) {
	for {
		s.out.prompt(question + " [" + fallback + "]:")
		answer, err := readLine(ctx)
		if err != nil {
			return "", err
		}
		if answer = strings.TrimSpace(answer); answer == "" {
			answer = fallback
		}
		if err := check(answer); errors.Is(err, errMissingTool) {
			return "", err
		} else if err != nil {
			s.out.line("⚠️", err.Error(), yellow)
			continue
		}
		return answer, nil
	}
}

// Confirmation: yes with --yes or in a dry run, otherwise asked
func (s *session) confirm(ctx context.Context, question string) (bool, error) {
	if s.yes || s.dryRun {
		return true, nil
	}
	return s.ask(ctx, question)
}

// y/N question on the terminal; No by default
func (s *session) ask(ctx context.Context, question string) (bool, error) {
	if !s.out.interactive {
		return false, errors.New("confirmation needs a terminal; pass --yes")
	}
	s.out.prompt(question + " [y/N]")
	answer, err := readLine(ctx)
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// Question without line break, so the answer follows on the same line
func (p printer) prompt(text string) {
	fmt.Fprint(p.out, "✏️ "+text+" ")
}

// One line from stdin, unbuffered so the menu still gets its keys; cancellable
func readLine(ctx context.Context) (string, error) {
	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)
	go func() {
		var line []byte
		buffer := make([]byte, 1)
		for {
			count, err := os.Stdin.Read(buffer)
			if count == 1 && buffer[0] == '\n' {
				lines <- result{string(line), nil}
				return
			}
			line = append(line, buffer[:count]...)
			if err != nil {
				lines <- result{"", errors.New("input closed")}
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case answer := <-lines:
		return answer.line, answer.err
	}
}
