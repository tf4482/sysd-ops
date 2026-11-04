//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Unit directories outside the home directory
const systemUnitDirectory = "/etc/systemd/system"

// Default values of the timer template, reproducing the client example
const (
	defaultOnBootSec          = "60s"
	defaultOnUnitInactiveSec  = "15s"
	defaultAccuracySec        = "100ms"
	defaultRandomizedDelaySec = "0"
	defaultInterpreter        = "/bin/bash"
)

// Precompiled input patterns
var (
	accountName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\$?$`)
	clockTime   = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// Everything an install needs, from the wizard or from options
type Plan struct {
	Entry   Entry
	Scope   string
	Kinds   []string
	Service RenderData
	Timer   RenderData
}

// Rendered unit file and its target path
type unitFile struct {
	kind    string
	path    string
	content string
}

// Unit directory of a scope
func (s *session) unitDirectory(scope string) string {
	if scope == scopeSystem {
		return s.systemUnits
	}
	return s.userUnits
}

// Unit kinds an install creates for the entry's state
func missingKinds(state string) []string {
	switch state {
	case stateServiceOnly:
		return []string{kindTimer}
	case stateTimerOnly:
		return []string{kindService}
	}
	return []string{kindService, kindTimer}
}

// Plan with default values for the entry; existing units fix the scope
func (s *session) basePlan(entry Entry, scope string) (Plan, error) {
	if entry.Scope != "" {
		scope = entry.Scope
	}
	interpreter, arguments, err := resolveInterpreter(entry.Script.Shebang)
	if err != nil {
		return Plan{}, err
	}
	words := []string{execWord(interpreter)}
	for _, word := range append(arguments, entry.Script.Target) {
		words = append(words, execWord(word))
	}
	name := entry.Script.Name
	data := RenderData{
		Name:               name,
		ScriptPath:         execWord(entry.Script.Target),
		Interpreter:        execWord(interpreter),
		ExecStart:          strings.Join(words, " "),
		WorkDir:            settingValue(filepath.Dir(entry.Script.Target)),
		Scope:              scope,
		OnBootSec:          defaultOnBootSec,
		OnUnitInactiveSec:  defaultOnUnitInactiveSec,
		Persistent:         true,
		AccuracySec:        defaultAccuracySec,
		RandomizedDelaySec: defaultRandomizedDelaySec,
		Created:            time.Now().Format(time.DateOnly),
		Tool:               projectName + " " + version(),
	}
	if scope == scopeSystem {
		data.User = s.account
	}
	plan := Plan{Entry: entry, Scope: scope, Kinds: missingKinds(entry.State), Service: data, Timer: data}
	plan.Service.Description = "Run " + settingValue(name)
	plan.Timer.Description = "Timer for " + settingValue(name)
	return plan, nil
}

// Interpreter path and its arguments from the shebang; env is resolved, no shebang means /bin/bash
func resolveInterpreter(shebang []string) (string, []string, error) {
	if len(shebang) == 0 {
		return defaultInterpreter, nil, nil
	}
	if filepath.Base(shebang[0]) != "env" {
		return shebang[0], shebang[1:], nil
	}
	command := shebangCommand(shebang)
	if command == "" {
		return "", nil, fmt.Errorf("cannot read the interpreter from shebang #!%s", strings.Join(shebang, " "))
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return "", nil, fmt.Errorf("interpreter %s from the shebang not found", command)
	}
	index := 1
	for shebang[index] != command {
		index++
	}
	return path, shebang[index+1:], nil
}

// Plan from command-line options; missing values keep their defaults
func (s *session) planFromOptions(ctx context.Context, entry Entry, options map[string]string) (Plan, error) {
	scope := s.config.InstallScope
	if value, ok := options["--scope"]; ok {
		scope = strings.ToLower(value)
		if scope != scopeUser && scope != scopeSystem {
			return Plan{}, fmt.Errorf("--scope must be user or system")
		}
	}
	plan, err := s.basePlan(entry, scope)
	if err != nil {
		return Plan{}, err
	}
	if value, ok := options["--user-account"]; ok && plan.Scope == scopeSystem {
		if !accountName.MatchString(value) {
			return Plan{}, fmt.Errorf("invalid account name: %s", value)
		}
		plan.Service.User, plan.Timer.User = value, value
	}
	timer := &plan.Timer
	_, boot := options["--on-boot"]
	_, inactive := options["--every-inactive"]
	_, active := options["--every-active"]
	preset, ok := options["--schedule"]
	if ok || boot || inactive || active {
		timer.OnBootSec, timer.OnUnitInactiveSec = "", ""
	}
	if ok {
		if err := applyPreset(timer, preset); err != nil {
			return Plan{}, err
		}
	}
	for option, field := range map[string]*string{
		"--on-boot": &timer.OnBootSec, "--every-inactive": &timer.OnUnitInactiveSec, "--every-active": &timer.OnUnitActiveSec,
	} {
		if value, ok := options[option]; ok {
			*field = value
		}
	}
	if !slices.Contains(plan.Kinds, kindTimer) {
		return plan, nil
	}
	if timer.OnCalendar == "" && timer.OnBootSec == "" && timer.OnUnitActiveSec == "" && timer.OnUnitInactiveSec == "" {
		return Plan{}, errors.New("the schedule has no trigger")
	}
	fields := s.templates[kindTimer].fields
	for field, value := range map[string]string{
		"OnCalendar": timer.OnCalendar, "OnBootSec": timer.OnBootSec,
		"OnUnitActiveSec": timer.OnUnitActiveSec, "OnUnitInactiveSec": timer.OnUnitInactiveSec,
	} {
		if value != "" && !fields[field] {
			return Plan{}, fmt.Errorf("timer template %s does not use .%s", s.templates[kindTimer].source, field)
		}
	}
	for _, span := range []string{timer.OnBootSec, timer.OnUnitActiveSec, timer.OnUnitInactiveSec} {
		if span != "" {
			if err := s.checkTimespan(ctx, span); err != nil {
				return Plan{}, err
			}
		}
	}
	if timer.OnCalendar != "" {
		next, err := s.checkCalendar(ctx, timer.OnCalendar)
		if err != nil {
			return Plan{}, err
		}
		s.out.line("🗓️", "Next elapse: "+next, "")
	}
	return plan, nil
}

// Timer fields of a schedule preset
func applyPreset(timer *RenderData, preset string) error {
	name, argument, _ := strings.Cut(preset, "@")
	if expression, ok := strings.CutPrefix(preset, "oncalendar:"); ok {
		timer.OnCalendar = expression
		return checkLine(expression)
	}
	switch {
	case preset == "default":
		timer.OnBootSec, timer.OnUnitInactiveSec = defaultOnBootSec, defaultOnUnitInactiveSec
	case preset == "hourly", preset == "monthly":
		timer.OnCalendar = preset
	case name == "daily" && validClock(argument):
		timer.OnCalendar = "*-*-* " + argument + ":00"
	case name == "weekly":
		day, clock, _ := strings.Cut(argument, "@")
		if !validWeekday(day) || !validClock(clock) {
			return fmt.Errorf("invalid schedule %s; use weekly@Mon@HH:MM", preset)
		}
		timer.OnCalendar = weekdayName(day) + " *-*-* " + clock + ":00"
	default:
		return fmt.Errorf("invalid schedule %s", preset)
	}
	return nil
}

// Weekday as systemd prints it, e.g. Mon
func weekdayName(day string) string {
	return strings.ToUpper(day[:1]) + strings.ToLower(day[1:])
}

// HH:MM on a 24-hour clock
func validClock(clock string) bool {
	return clockTime.MatchString(clock)
}

// Three-letter English weekday, any case
func validWeekday(day string) bool {
	return len(day) == 3 && strings.Contains("mon tue wed thu fri sat sun", strings.ToLower(day))
}

// Values must stay on one line so they cannot add directives
func checkLine(value string) error {
	if strings.ContainsFunc(value, func(character rune) bool { return character < ' ' || character == 0x7f }) {
		return errors.New("values must not contain control characters")
	}
	return nil
}

// Validate an OnCalendar= expression; returns the next elapse text
func (s *session) checkCalendar(ctx context.Context, expression string) (string, error) {
	if err := checkLine(expression); err != nil {
		return "", err
	}
	if err := analyzeAvailable(); err != nil {
		return "", err
	}
	output, err := s.query(ctx, "systemd-analyze", "calendar", "--", expression)
	if err != nil {
		return "", fmt.Errorf("invalid OnCalendar expression %q", expression)
	}
	for line := range strings.SplitSeq(output, "\n") {
		if next, ok := strings.CutPrefix(strings.TrimSpace(line), "Next elapse:"); ok {
			return strings.TrimSpace(next), nil
		}
	}
	return "unknown", nil
}

// Validate a time span such as 60s or 15min
func (s *session) checkTimespan(ctx context.Context, span string) error {
	if err := checkLine(span); err != nil {
		return err
	}
	if err := analyzeAvailable(); err != nil {
		return err
	}
	if _, err := s.query(ctx, "systemd-analyze", "timespan", "--", span); err != nil {
		return fmt.Errorf("invalid time span %q", span)
	}
	return nil
}

// Validation needs systemd-analyze; asking again cannot fix its absence
func analyzeAvailable() error {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		return fmt.Errorf("%w: systemd-analyze", errMissingTool)
	}
	return nil
}

// Render the planned units, refusing any that already exist
func (s *session) renderPlan(ctx context.Context, plan Plan) ([]unitFile, error) {
	var files []unitFile
	for _, kind := range plan.Kinds {
		data := plan.Service
		if kind == kindTimer {
			data = plan.Timer
		}
		for _, value := range []string{data.Description, data.User, data.AccuracySec, data.RandomizedDelaySec} {
			if err := checkLine(value); err != nil {
				return nil, err
			}
		}
		if plan.Scope == scopeSystem && data.User != "" && !accountName.MatchString(data.User) {
			return nil, fmt.Errorf("invalid account name: %s", data.User)
		}
		content, err := s.templates[kind].render(data)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(s.unitDirectory(plan.Scope), plan.Entry.Script.Name+"."+kind)
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s already exists; existing units are never overwritten", path)
		}
		files = append(files, unitFile{kind, path, content})
	}
	return files, nil
}

// Refuse units of the target scope that systemd already knows, wherever their files live
func (s *session) checkTargetScope(ctx context.Context, plan Plan) error {
	var names []string
	for _, kind := range plan.Kinds {
		names = append(names, plan.Entry.Script.Name+"."+kind)
	}
	units, err := s.showUnits(ctx, plan.Scope, names)
	if err != nil {
		return err
	}
	for _, unit := range units {
		if unit.exists() {
			return fmt.Errorf("%s already exists in %s scope at %s; existing units are never overwritten", unit.Name, plan.Scope, orDash(unit.FragmentPath))
		}
	}
	return nil
}

// systemd-analyze verify on copies in a temporary directory, when available
func (s *session) verifyUnits(ctx context.Context, scope string, files []unitFile) error {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		s.out.line("⚠️", "systemd-analyze not found · units not verified", yellow)
		return nil
	}
	directory, err := os.MkdirTemp("", projectName+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	arguments := append([]string{"verify", "--man=no"}, scopeArguments(scope)...)
	for _, file := range files {
		path := filepath.Join(directory, filepath.Base(file.path))
		if err := os.WriteFile(path, []byte(file.content), 0o600); err != nil {
			return err
		}
		arguments = append(arguments, path)
	}
	_, err = s.query(ctx, "systemd-analyze", arguments...)
	return err
}

// Script must be executable when ExecStart= runs it directly; true when chmod is needed and accepted
func (s *session) needsChmod(ctx context.Context, plan Plan, files []unitFile) (bool, error) {
	for _, file := range files {
		if file.kind != kindService || execProgram(execStartLine(file.content)) != plan.Entry.Script.Target {
			continue
		}
		info, err := os.Stat(plan.Entry.Script.Target)
		if err != nil {
			return false, err
		}
		if info.Mode()&0o100 != 0 {
			return false, nil
		}
		ok, err := s.confirm(ctx, "ExecStart= runs the script directly, but it is not executable. Run chmod u+x?")
		if err != nil {
			return false, err
		}
		if !ok {
			return false, errors.New("script is not executable; install aborted")
		}
		return true, nil
	}
	return false, nil
}

// Add the owner execute bit to the script
func (s *session) makeExecutable(path string) error {
	if s.dryRun {
		s.out.line("⏭️", "Would run: chmod u+x "+path, yellow)
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.Chmod(path, info.Mode().Perm()|0o100)
}

// Steps 3 to 10 of the install flow; failures offer to remove what was created
func (s *session) install(ctx context.Context, plan Plan) error {
	if err := s.checkTargetScope(ctx, plan); err != nil {
		return err
	}
	files, err := s.renderPlan(ctx, plan)
	if err != nil {
		return err
	}
	chmod, err := s.needsChmod(ctx, plan, files)
	if err != nil {
		return err
	}
	for _, file := range files {
		s.out.line("📄", file.path, cyan)
		for line := range strings.SplitSeq(strings.TrimSuffix(file.content, "\n"), "\n") {
			s.out.print(strings.TrimRight("   "+line, " "), "")
		}
	}
	if ok, err := s.confirm(ctx, "Create and enable these units?"); err != nil || !ok {
		if err == nil {
			err = errAborted
		}
		return err
	}
	// The execute bit comes first, since verify rejects a script that is not executable
	if chmod {
		if err := s.makeExecutable(plan.Entry.Script.Target); err != nil {
			return err
		}
	}
	if err := s.verifyUnits(ctx, plan.Scope, files); err != nil {
		if chmod {
			err = fmt.Errorf("%v (script made executable)", err)
		}
		return err
	}

	var created []string
	enabled := false
	ownTimer := slices.Contains(plan.Kinds, kindTimer)
	timer := plan.Entry.Script.Name + ".timer"
	fail := func(err error) error { return s.undoInstall(ctx, plan.Scope, timer, created, enabled, err) }
	for _, file := range files {
		if err := s.writeUnit(ctx, plan.Scope, file); err != nil {
			return fail(err)
		}
		created = append(created, file.path)
	}
	if err := s.systemctl(ctx, plan.Scope, "daemon-reload"); err != nil {
		return fail(err)
	}
	// Only a timer created here may be disabled again by the cleanup
	enabled = ownTimer
	if err := s.systemctl(ctx, plan.Scope, "enable", "--now", timer); err != nil {
		return fail(err)
	}
	if s.dryRun {
		return nil
	}
	units, err := s.showUnits(ctx, plan.Scope, []string{timer})
	if err != nil {
		return fail(err)
	}
	if units[0].ActiveState != "active" {
		return fail(fmt.Errorf("%s is %s after enabling", timer, units[0].ActiveState))
	}
	s.out.line("✅", strings.Join([]string{timer, "active", units[0].UnitFileState, "next " + orDash(units[0].NextElapse)}, " · "), green)
	if plan.Scope == scopeUser {
		s.lingerHint(ctx)
	}
	return nil
}

// Write one new unit file without overwriting
func (s *session) writeUnit(ctx context.Context, scope string, file unitFile) error {
	if s.dryRun {
		s.out.line("⏭️", "Would write "+file.path, yellow)
		return nil
	}
	if scope == scopeSystem {
		temporary, err := writeTemporary("", file.content)
		if err != nil {
			return err
		}
		defer os.Remove(temporary)
		if _, err := os.Lstat(file.path); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s already exists; existing units are never overwritten", file.path)
		}
		return s.privileged(ctx, "install", "-m", "0644", "--", temporary, file.path)
	}
	if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
		return err
	}
	temporary, err := writeTemporary(filepath.Dir(file.path), file.content)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := os.Chmod(temporary, 0o644); err != nil {
		return err
	}
	// A hard link fails when the target exists, so nothing is overwritten
	if err := os.Link(temporary, file.path); err != nil {
		return fmt.Errorf("cannot create %s: %v", file.path, err)
	}
	return nil
}

// Private temporary file with the content; returns its path
func writeTemporary(directory, content string) (string, error) {
	file, err := os.CreateTemp(directory, "."+projectName+"-*")
	if err != nil {
		return "", err
	}
	_, err = file.WriteString(content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// Report a failed install and offer removing the files just created
func (s *session) undoInstall(ctx context.Context, scope, timer string, created []string, enabled bool, cause error) error {
	if len(created) == 0 {
		return cause
	}
	done := "created: " + strings.Join(created, ", ")
	if enabled {
		done += "; enabled " + timer
	}
	report := fmt.Errorf("%v (%s)", cause, done)
	if ctx.Err() != nil {
		return report
	}
	s.out.line("❌", "Install error: "+cause.Error(), red)
	if ok, err := s.confirm(ctx, "Remove the files just created?"); err != nil || !ok {
		return report
	}
	if enabled {
		if err := s.systemctl(ctx, scope, "disable", "--now", timer); err != nil {
			return fmt.Errorf("%v; cleanup failed: %v", report, err)
		}
	}
	for _, path := range created {
		if err := s.removePath(ctx, scope, path, false); err != nil {
			return fmt.Errorf("%v; cleanup failed: %v", report, err)
		}
	}
	if err := s.systemctl(ctx, scope, "daemon-reload"); err != nil {
		return fmt.Errorf("%v; cleanup failed: %v", report, err)
	}
	return fmt.Errorf("%v; created files removed", cause)
}

// Hint at lingering unless loginctl reports it enabled
func (s *session) lingerHint(ctx context.Context) {
	output, err := s.query(ctx, "loginctl", "show-user", "--property=Linger", "--", s.account)
	if err == nil && strings.TrimSpace(output) == "Linger=yes" {
		return
	}
	s.out.line("💡", "To run the timer without a login session: loginctl enable-linger "+s.account, "")
}

// Remove a unit file or drop-in directory; sudo in system scope
func (s *session) removePath(ctx context.Context, scope, path string, directory bool) error {
	if s.dryRun {
		s.out.line("⏭️", "Would remove "+path, yellow)
		return nil
	}
	if scope == scopeSystem {
		flags := "-f"
		if directory {
			flags = "-rf"
		}
		return s.privileged(ctx, "rm", flags, "--", path)
	}
	if directory {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// Fragment path may be deleted only when it is the unit file directly in the scope's directory
func deletableFragment(fragment, directory, name string) bool {
	return filepath.IsAbs(fragment) && filepath.Clean(fragment) == filepath.Join(directory, name)
}

// Uninstall the timer, then optionally the service; the script stays untouched
func (s *session) uninstall(ctx context.Context, entry Entry) error {
	if !entry.Timer.exists() {
		return fmt.Errorf("%s has no timer to uninstall", entry.Script.Name)
	}
	if err := s.removeUnit(ctx, entry.Scope, *entry.Timer, ""); err != nil {
		return err
	}
	if !entry.Service.exists() {
		return nil
	}
	ok := false
	if s.out.interactive && !s.dryRun {
		var err error
		if ok, err = s.ask(ctx, "Also uninstall "+entry.Service.Name+"?"); err != nil {
			return err
		}
	}
	if !ok {
		s.out.line("⏭️", entry.Service.Name+" kept", yellow)
		return nil
	}
	return s.removeUnit(ctx, entry.Scope, *entry.Service, "confirmed")
}

// Disable and remove one unit; package units are only disabled and may be masked
func (s *session) removeUnit(ctx context.Context, scope string, unit Unit, confirmed string) error {
	directory := s.unitDirectory(scope)
	deletable := deletableFragment(unit.FragmentPath, directory, unit.Name)
	dropins := filepath.Join(directory, unit.Name+".d")
	s.out.line("📄", unit.Name+" · "+scope+" · "+orDash(unit.FragmentPath), cyan)
	if unit.DropInPaths != "" {
		s.out.line("📎", "Drop-ins: "+unit.DropInPaths, "")
	}
	if deletable {
		s.out.line("🗑️", "Will disable and stop it, remove the unit file and "+dropins+", and reload systemd", "")
	} else {
		s.out.line("⚠️", "Not in "+directory+" (package or foreign unit): will only disable and stop it, then offer masking", yellow)
	}
	if confirmed == "" {
		if ok, err := s.confirm(ctx, "Uninstall "+unit.Name+"?"); err != nil || !ok {
			if err == nil {
				err = errAborted
			}
			return err
		}
	}

	var done []string
	fail := func(err error) error {
		if len(done) == 0 {
			return err
		}
		return fmt.Errorf("%v (done: %s)", err, strings.Join(done, ", "))
	}
	if err := s.systemctl(ctx, scope, "disable", "--now", unit.Name); err != nil {
		return fail(err)
	}
	done = append(done, "disabled "+unit.Name)
	if deletable {
		if err := s.removePath(ctx, scope, unit.FragmentPath, false); err != nil {
			return fail(err)
		}
		done = append(done, "removed "+unit.FragmentPath)
		if _, err := os.Lstat(dropins); err == nil {
			if err := s.removePath(ctx, scope, dropins, true); err != nil {
				return fail(err)
			}
			done = append(done, "removed "+dropins)
		}
	} else {
		s.out.line("💡", "Package units are restored by updates; masking links the unit to /dev/null so it cannot start", "")
		ok := false
		if s.out.interactive && !s.dryRun {
			var err error
			if ok, err = s.ask(ctx, "Mask "+unit.Name+"?"); err != nil {
				return fail(err)
			}
		}
		if ok {
			if err := s.systemctl(ctx, scope, "mask", unit.Name); err != nil {
				return fail(err)
			}
			done = append(done, "masked "+unit.Name)
		}
	}
	if err := s.systemctl(ctx, scope, "daemon-reload"); err != nil {
		return fail(err)
	}
	// Fails for units no longer loaded, which have no failed state left
	_ = s.systemctl(ctx, scope, "reset-failed", unit.Name)
	s.out.line("✅", unit.Name+" uninstalled", green)
	return nil
}

// Open a unit with systemctl edit and the configured editor
func (s *session) edit(ctx context.Context, scope, unit string, dropin bool) error {
	if !s.out.interactive {
		return errors.New("editing needs a terminal")
	}
	editor, err := exec.LookPath(s.config.Editor)
	if err != nil {
		return fmt.Errorf("editor %s not found; install it or set settings.editor in the configuration", s.config.Editor)
	}
	arguments := []string{"edit"}
	if !dropin {
		arguments = append(arguments, "--full")
	}
	arguments = append(arguments, unit)
	if scope == scopeSystem {
		return s.privileged(ctx, append([]string{"env", "SYSTEMD_EDITOR=" + editor, "systemctl"}, arguments...)...)
	}
	if s.dryRun {
		s.out.line("⏭️", "Would run: SYSTEMD_EDITOR="+editor+" "+commandText("systemctl --user", arguments), yellow)
		return nil
	}
	return s.attached(ctx, []string{"SYSTEMD_EDITOR=" + editor}, "systemctl", append([]string{"--user"}, arguments...)...)
}

// Dash for empty values in the output
func orDash(value string) string {
	if value == "" {
		return "–"
	}
	return value
}
