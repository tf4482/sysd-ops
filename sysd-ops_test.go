//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Configuration matching the embedded template
const validConfig = defaultConfig

// Client example timer, reproduced byte for byte by the default template
const exampleTimer = `[Unit]
Description=Timing network related checks

[Timer]
OnBootSec=60s
OnUnitInactiveSec=15s
AccuracySec=100ms
RandomizedDelaySec=0

[Install]
WantedBy=timers.target
`

// Client example service, reproduced byte for byte by the default template
const exampleService = `[Unit]
Description=Performing network related checks
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
User=example
ExecStart=/bin/bash /home/example/bin/check
TimeoutStartSec=2min
PrivateTmp=yes
`

// Fake systemd tools: every call is logged; units are loaded once enabled or listed in $LOADED
const (
	fakeSystemctl = `case "$*" in
*enable*) : > "$STATE" ;;
*" show "*|show*)
  for unit; do
    case "$unit" in
    *.timer|*.service)
      state=not-found
      if [ -e "$STATE" ] || [ "$LOADED" = "$unit" ]; then state=loaded; fi
      printf 'LoadState=%s\nActiveState=active\nUnitFileState=enabled\nNextElapseUSecRealtime=soon\n\n' "$state" ;;
    esac
  done ;;
esac`
	fakeAnalyze = `case "$*" in
*bogus*) echo "Failed to parse" >&2; exit 1 ;;
calendar*) printf '  Next elapse: Mon 2026-11-09 07:00:00 UTC\n' ;;
esac`
	fakeLoginctl = `printf 'Linger=yes\n'`
)

// Write a file and its directories
func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// Replace PATH with fake tools; returns a reader for the call log
func fakeTools(t *testing.T, tools map[string]string) func() []string {
	t.Helper()
	directory := t.TempDir()
	log := filepath.Join(directory, "calls.log")
	for name, body := range tools {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + log + "\n" + body + "\n"
		writeFile(t, filepath.Join(directory, "bin", name), script, 0o755)
	}
	t.Setenv("PATH", filepath.Join(directory, "bin"))
	t.Setenv("STATE", filepath.Join(directory, "enabled"))
	return func() []string {
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// Default fake tools for install and uninstall flows
func defaultTools(t *testing.T) func() []string {
	t.Helper()
	return fakeTools(t, map[string]string{
		"systemctl": fakeSystemctl, "systemd-analyze": fakeAnalyze, "loginctl": fakeLoginctl, "sudo": "",
	})
}

// Session with built-in templates and unit directories in a temp directory
func testSession(t *testing.T) (*session, *bytes.Buffer) {
	t.Helper()
	templates, err := loadTemplates(nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output := &bytes.Buffer{}
	return &session{
		out:         printer{out: output},
		config:      Config{Editor: "nano", InstallScope: scopeUser, CommandTimeout: 5 * time.Second},
		directory:   root,
		scopes:      []string{scopeUser},
		templates:   templates,
		userUnits:   filepath.Join(root, "user"),
		systemUnits: filepath.Join(root, "system"),
		account:     "example",
		yes:         true,
	}, output
}

// Script entry for an existing file
func testEntry(t *testing.T, s *session, state string) Entry {
	t.Helper()
	path := filepath.Join(s.directory, "backup.sh")
	writeFile(t, path, "#!/bin/bash\necho ok\n", 0o755)
	entry := Entry{Script: Script{"backup", path, path, []string{"/bin/bash"}}, State: state}
	if state != stateScriptOnly {
		entry.Scope = scopeUser
	}
	return entry
}

// Log lines must start with the expected commands, in order
func checkCalls(t *testing.T, calls []string, expected ...string) {
	t.Helper()
	if len(calls) != len(expected) {
		t.Fatalf("calls = %q, want %d calls %q", calls, len(expected), expected)
	}
	for index, prefix := range expected {
		if !strings.HasPrefix(calls[index], prefix) {
			t.Errorf("call %d = %q, want prefix %q", index, calls[index], prefix)
		}
	}
}

func TestFindScripts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "backup.sh"), "echo no shebang\n", 0o644)
	writeFile(t, filepath.Join(root, "clean"), "#!/usr/bin/env bash\n", 0o755)
	writeFile(t, filepath.Join(root, "fishy"), "#!/usr/bin/fish\n", 0o755)
	writeFile(t, filepath.Join(root, "tool"), "#!/usr/bin/python3\n", 0o755)
	writeFile(t, filepath.Join(root, "notes.txt"), "#!/bin/sh\n", 0o644)
	writeFile(t, filepath.Join(root, "long"), strings.Repeat("x", 300)+"\n#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(root, "sub", "deep.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(root, ".hidden", "secret.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(t.TempDir(), "target"), "", 0o644)
	if err := os.Symlink(filepath.Join(root, "clean"), filepath.Join(root, "linked.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "dir.sh")); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		recursive bool
		names     []string
	}{
		"flat":      {false, []string{"backup", "clean", "fishy", "linked"}},
		"recursive": {true, []string{"backup", "clean", "deep", "fishy", "linked"}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			scripts, err := findScripts(root, test.recursive)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, script := range scripts {
				names = append(names, script.Name)
			}
			if !slices.Equal(names, test.names) {
				t.Errorf("names = %v, want %v", names, test.names)
			}
			linked := scripts[slices.IndexFunc(scripts, func(script Script) bool { return script.Name == "linked" })]
			if linked.Target != filepath.Join(root, "clean") {
				t.Errorf("symlink target = %s", linked.Target)
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	cases := map[string][2]string{
		"unknown key":    {`recursive: false`, "recursive: false\n  extra: 1"},
		"missing key":    {`  recursive: false` + "\n", ""},
		"bad scope":      {`scope: "all"`, `scope: "both"`},
		"bad install":    {`install_scope: "user"`, `install_scope: "all"`},
		"bad recursive":  {`recursive: false`, `recursive: "no"`},
		"empty editor":   {`editor: "nano"`, `editor: ""`},
		"bad timeout":    {`command_timeout_seconds: 30`, `command_timeout_seconds: 0`},
		"directory type": {`directory: ""`, `directory: 5`},
	}
	parse := func(text string) (Config, error) {
		var document any
		if err := yaml.Unmarshal([]byte(text), &document); err != nil {
			t.Fatal(err)
		}
		return parseConfig(document)
	}
	config, err := parse(validConfig)
	if err != nil || config.Editor != "nano" || config.Scope != scopeAll || config.CommandTimeout != 30*time.Second {
		t.Fatalf("valid config = %+v, %v", config, err)
	}
	for name, replace := range cases {
		if _, err := parse(strings.Replace(validConfig, replace[0], replace[1], 1)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	binary, home := t.TempDir(), t.TempDir()
	userPath := filepath.Join(home, ".config", projectName, configFilename)

	// First run writes a private template
	_, _, err := loadConfig(binary, home)
	var created *configCreatedError
	if !errors.As(err, &created) {
		t.Fatalf("first run error = %v", err)
	}
	if info, err := os.Stat(userPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("template mode = %v, %v", info, err)
	}
	if path, _, err := loadConfig(binary, home); err != nil || path != userPath {
		t.Fatalf("template rejected: %s, %v", path, err)
	}

	// The file beside the binary wins; a broken one does not fall back or leak content
	writeFile(t, filepath.Join(binary, configFilename), strings.Replace(validConfig, `editor: "nano"`, `editor: "vim"`, 1), 0o600)
	if _, config, err := loadConfig(binary, home); err != nil || config.Editor != "vim" {
		t.Fatalf("binary config = %+v, %v", config, err)
	}
	writeFile(t, filepath.Join(binary, configFilename), "settings: [secret-value\n", 0o600)
	_, _, err = loadConfig(binary, home)
	if err == nil || strings.Contains(err.Error(), "secret-value") || !strings.Contains(err.Error(), filepath.Join(binary, configFilename)) {
		t.Fatalf("broken config error = %v", err)
	}
}

func TestDirectoryPriority(t *testing.T) {
	home, flagDir, configDir, workdir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Chdir(workdir)
	cases := map[string]struct {
		options   map[string]string
		directory string
		want      string
		source    string
	}{
		"flag":   {map[string]string{"--dir": flagDir}, configDir, flagDir, "flag"},
		"config": {map[string]string{}, configDir, configDir, "config"},
		"cwd":    {map[string]string{}, "", workdir, "working directory"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := newSession(printer{out: &bytes.Buffer{}}, Config{Directory: test.directory, Scope: scopeAll}, test.options)
			if err != nil {
				t.Fatal(err)
			}
			if s.directory != test.want || s.directorySource != test.source {
				t.Errorf("directory = %s from %s", s.directory, s.directorySource)
			}
		})
	}
	options := map[string]string{"--user": "", "--system": ""}
	var usage *invalidUsage
	if _, err := newSession(printer{out: &bytes.Buffer{}}, Config{Scope: scopeAll}, options); !errors.As(err, &usage) {
		t.Errorf("conflicting scopes error = %v", err)
	}
}

func TestParseShow(t *testing.T) {
	cases := map[string]struct {
		output string
		blocks []map[string]string
		fails  bool
	}{
		"two units": {
			"Id=a.timer\nLoadState=loaded\nDropInPaths=\n\nId=a.service\nLoadState=not-found\n",
			[]map[string]string{{"Id": "a.timer", "LoadState": "loaded", "DropInPaths": ""}, {"Id": "a.service", "LoadState": "not-found"}},
			false,
		},
		"value with equals": {"ExecStart=x=y\n", []map[string]string{{"ExecStart": "x=y"}}, false},
		"empty":             {"\n\n", nil, false},
		"garbage":           {"LoadState loaded\n", nil, true},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			blocks, err := parseShow(test.output)
			if (err != nil) != test.fails {
				t.Fatalf("error = %v", err)
			}
			if len(blocks) != len(test.blocks) {
				t.Fatalf("blocks = %v", blocks)
			}
			for index := range blocks {
				for key, value := range test.blocks[index] {
					if blocks[index][key] != value {
						t.Errorf("block %d %s = %q, want %q", index, key, blocks[index][key], value)
					}
				}
			}
		})
	}
}

func TestBuildEntries(t *testing.T) {
	loaded := func(name string) Unit { return Unit{Name: name, LoadState: "loaded"} }
	units := map[string]map[string]Unit{scopeUser: {
		"full.timer": loaded("full.timer"), "full.service": loaded("full.service"),
		"svc.service":   loaded("svc.service"),
		"tmr.timer":     loaded("tmr.timer"),
		"gone.timer":    {Name: "gone.timer", LoadState: "not-found"},
		"tpl@.timer":    loaded("tpl@.timer"),
		"tpl@x.timer":   loaded("tpl@x.timer"),
		"backup2.timer": loaded("backup2.timer"),
		"dup.timer":     loaded("dup.timer"),
	}}
	scripts := []Script{
		{Name: "backup"}, {Name: "bad name"}, {Name: "dup", Path: "/a/dup.sh"}, {Name: "dup", Path: "/b/dup.sh"},
		{Name: "full"}, {Name: "gone"}, {Name: "svc"}, {Name: "tmr"}, {Name: "tpl"},
	}
	want := map[string][2]string{
		"backup": {stateScriptOnly, ""}, "bad name": {stateScriptOnly, "not installable"},
		"dup": {stateTimerOnly, "ambiguous"}, "full": {stateComplete, ""}, "gone": {stateScriptOnly, ""},
		"svc": {stateServiceOnly, ""}, "tmr": {stateTimerOnly, ""}, "tpl": {stateScriptOnly, ""},
	}
	entries := buildEntries(scripts, units, []string{scopeUser})
	if len(entries) != len(scripts) {
		t.Fatalf("entries = %d, want %d", len(entries), len(scripts))
	}
	for _, entry := range entries {
		expected := want[entry.Script.Name]
		if entry.State != expected[0] || !strings.HasPrefix(entry.Problem, expected[1]) || (expected[1] == "") != (entry.Problem == "") {
			t.Errorf("%s = %s %q, want %v", entry.Script.Name, entry.State, entry.Problem, expected)
		}
		if entry.Problem != "" && entry.actions() != nil {
			t.Errorf("%s offers actions despite a problem", entry.Script.Name)
		}
	}
}

func TestExampleGolden(t *testing.T) {
	s, _ := testSession(t)
	entry := Entry{Script: Script{"check", "/home/example/bin/check", "/home/example/bin/check", []string{"/bin/bash"}}, State: stateScriptOnly}
	plan, err := s.basePlan(entry, scopeSystem)
	if err != nil {
		t.Fatal(err)
	}
	plan.Service.Description = "Performing network related checks"
	plan.Timer.Description = "Timing network related checks"
	for kind, expected := range map[string]string{kindService: exampleService, kindTimer: exampleTimer} {
		data := plan.Service
		if kind == kindTimer {
			data = plan.Timer
		}
		got, err := s.templates[kind].render(data)
		if err != nil {
			t.Fatal(err)
		}
		if got != expected {
			t.Errorf("%s differs from the client example:\n%s", kind, got)
		}
	}
}

func TestScheduleGolden(t *testing.T) {
	s, _ := testSession(t)
	defaults := "AccuracySec=100ms\nRandomizedDelaySec=0\n"
	cases := map[string]struct {
		options map[string]string
		timer   string
	}{
		"default":    {map[string]string{}, "OnBootSec=60s\nOnUnitInactiveSec=15s\n"},
		"hourly":     {map[string]string{"--schedule": "hourly"}, "OnCalendar=hourly\nPersistent=true\n"},
		"daily":      {map[string]string{"--schedule": "daily@03:30"}, "OnCalendar=*-*-* 03:30:00\nPersistent=true\n"},
		"weekly":     {map[string]string{"--schedule": "weekly@mon@07:00"}, "OnCalendar=Mon *-*-* 07:00:00\nPersistent=true\n"},
		"monthly":    {map[string]string{"--schedule": "monthly"}, "OnCalendar=monthly\nPersistent=true\n"},
		"oncalendar": {map[string]string{"--schedule": "oncalendar:Sat 12:00"}, "OnCalendar=Sat 12:00\nPersistent=true\n"},
		"boot":       {map[string]string{"--on-boot": "5min"}, "OnBootSec=5min\n"},
		"inactive":   {map[string]string{"--every-inactive": "1h", "--on-boot": "1min"}, "OnBootSec=1min\nOnUnitInactiveSec=1h\n"},
		"active":     {map[string]string{"--every-active": "2h"}, "OnUnitActiveSec=2h\n"},
		"combined":   {map[string]string{"--schedule": "hourly", "--on-boot": "1min"}, "OnCalendar=hourly\nPersistent=true\nOnBootSec=1min\n"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			defaultTools(t)
			plan, err := s.planFromOptions(context.Background(), Entry{Script: Script{Name: "job", Target: "/srv/job.sh"}}, test.options)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.templates[kindTimer].render(plan.Timer)
			if err != nil {
				t.Fatal(err)
			}
			want := "[Unit]\nDescription=Timer for job\n\n[Timer]\n" + test.timer + defaults + "\n[Install]\nWantedBy=timers.target\n"
			if got != want {
				t.Errorf("timer =\n%s\nwant\n%s", got, want)
			}
		})
	}
	defaultTools(t)
	for _, options := range []map[string]string{{"--schedule": "oncalendar:bogus"}, {"--schedule": "daily@25:00"}, {"--on-boot": "bogus"}} {
		if _, err := s.planFromOptions(context.Background(), Entry{Script: Script{Name: "job"}}, options); err == nil {
			t.Errorf("%v: expected an error", options)
		}
	}
}

func TestServiceEscaping(t *testing.T) {
	s, _ := testSession(t)
	path := "/home/u/my scripts/50% $HOME \"x\".sh"
	plan, err := s.basePlan(Entry{Script: Script{Name: "odd", Target: path, Shebang: []string{"/bin/sh", "-e"}}}, scopeUser)
	if err != nil {
		t.Fatal(err)
	}
	plan.Service.Description = "Run 100%"
	got, err := s.templates[kindService].render(plan.Service)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "User=") {
		t.Error("user unit sets User=")
	}
	want := `ExecStart=/bin/sh -e "/home/u/my scripts/50%% $$HOME \"x\".sh"`
	if !strings.Contains(got, want+"\n") {
		t.Errorf("service =\n%s\nwant line %s", got, want)
	}
	if plan.Service.ScriptPath != execWord(path) {
		t.Errorf("ScriptPath = %s", plan.Service.ScriptPath)
	}
	if command := shebangCommand([]string{"/usr/bin/env", "-u", "FOO", "-S", "bash", "-e"}); command != "bash" {
		t.Errorf("env command = %s", command)
	}
	if program := execProgram(execWord(path) + " arg"); program != path {
		t.Errorf("execProgram = %q", program)
	}
}

func TestTemplates(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(first, "timer.tmpl"), "[Unit]\n[Timer]\nOnCalendar={{.OnCalendar}}\n", 0o644)
	writeFile(t, filepath.Join(second, "timer.tmpl"), "[Unit]\n[Timer]\n", 0o644)
	writeFile(t, filepath.Join(second, "service.tmpl"), "[Unit]\n[Service]\nWorkingDirectory={{.WorkDir}}\n", 0o644)

	templates, err := loadTemplates([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if templates[kindTimer].source != filepath.Join(first, "timer.tmpl") || templates[kindService].source != filepath.Join(second, "service.tmpl") {
		t.Errorf("sources = %s, %s", templates[kindTimer].source, templates[kindService].source)
	}
	if !templates[kindService].fields["WorkDir"] || templates[kindService].fields["User"] {
		t.Errorf("custom fields = %v", templates[kindService].fields)
	}
	builtin, err := loadTemplates([]string{t.TempDir()})
	if err != nil || builtin[kindService].source != builtinSource {
		t.Fatalf("built-in = %v, %v", builtin[kindService].source, err)
	}
	fields := builtin[kindService].fields
	if !fields["ExecStart"] || !fields["User"] || !fields["Scope"] || fields["WorkDir"] {
		t.Errorf("built-in service fields = %v", fields)
	}

	cases := map[string]string{
		"unknown field": "[Unit]\n\n{{if .Scope}}{{.Nope}}{{end}}\n",
		"syntax":        "[Unit]\n{{if .Scope}\n",
	}
	for name, text := range cases {
		_, err := parseTemplate(kindService, "/x/service.tmpl", text)
		if err == nil || !strings.Contains(err.Error(), "/x/service.tmpl:") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	if _, err := (unitTemplate{kindTimer, "t", templates[kindService].template, nil}).render(RenderData{}); err == nil {
		t.Error("a service template rendered as timer must miss [Timer]")
	}
}

func TestInstallFlow(t *testing.T) {
	cases := map[string]struct {
		state string
		files []string
	}{
		stateScriptOnly:  {stateScriptOnly, []string{"backup.service", "backup.timer"}},
		stateServiceOnly: {stateServiceOnly, []string{"backup.timer"}},
		stateTimerOnly:   {stateTimerOnly, []string{"backup.service"}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			calls := defaultTools(t)
			s, _ := testSession(t)
			plan, err := s.basePlan(testEntry(t, s, test.state), scopeUser)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.install(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(s.userUnits)
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if !slices.Equal(names, test.files) {
				t.Errorf("files = %v, want %v", names, test.files)
			}
			info, _ := os.Stat(filepath.Join(s.userUnits, test.files[0]))
			if info.Mode().Perm() != 0o644 {
				t.Errorf("mode = %v", info.Mode())
			}
			checkCalls(t, calls(), "systemctl --user show ", "systemd-analyze verify --man=no --user ", "systemctl --user daemon-reload",
				"systemctl --user enable --now backup.timer", "systemctl --user show ", "loginctl show-user --property=Linger -- example")
		})
	}
}

func TestInstallSystemScope(t *testing.T) {
	calls := defaultTools(t)
	s, _ := testSession(t)
	plan, err := s.basePlan(testEntry(t, s, stateScriptOnly), scopeSystem)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, calls(), "systemctl show ", "systemd-analyze verify --man=no /",
		"sudo install -m 0644 -- ", "sudo install -m 0644 -- ",
		"sudo systemctl daemon-reload", "sudo systemctl enable --now backup.timer", "systemctl show ")
}

func TestInstallRefusals(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		calls := defaultTools(t)
		s, _ := testSession(t)
		writeFile(t, filepath.Join(s.userUnits, "backup.timer"), "keep", 0o644)
		plan, _ := s.basePlan(testEntry(t, s, stateScriptOnly), scopeUser)
		if err := s.install(context.Background(), plan); err == nil {
			t.Fatal("expected an error")
		}
		if data, _ := os.ReadFile(filepath.Join(s.userUnits, "backup.timer")); string(data) != "keep" {
			t.Error("existing unit was overwritten")
		}
		if _, err := os.Stat(filepath.Join(s.userUnits, "backup.service")); err == nil {
			t.Error("service written despite the conflict")
		}
		checkCalls(t, calls(), "systemctl --user show ")
	})
	t.Run("unit known elsewhere in scope", func(t *testing.T) {
		calls := defaultTools(t)
		t.Setenv("LOADED", "backup.service")
		s, _ := testSession(t)
		plan, _ := s.basePlan(testEntry(t, s, stateScriptOnly), scopeSystem)
		if err := s.install(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "already exists in system scope") {
			t.Fatalf("error = %v", err)
		}
		checkCalls(t, calls(), "systemctl show ")
	})
	t.Run("no confirmation", func(t *testing.T) {
		calls := defaultTools(t)
		s, _ := testSession(t)
		s.yes = false
		plan, _ := s.basePlan(testEntry(t, s, stateScriptOnly), scopeUser)
		if err := s.install(context.Background(), plan); err == nil {
			t.Fatal("expected an error")
		}
		if _, err := os.Stat(s.userUnits); err == nil {
			t.Error("files written without confirmation")
		}
		checkCalls(t, calls(), "systemctl --user show ")
	})
	t.Run("failed enable cleans up", func(t *testing.T) {
		calls := fakeTools(t, map[string]string{
			"systemctl":       `case "$*" in *enable*) echo "boom" >&2; exit 1 ;; *show*) printf 'LoadState=not-found\n\nLoadState=not-found\n' ;; esac`,
			"systemd-analyze": fakeAnalyze,
		})
		s, _ := testSession(t)
		plan, _ := s.basePlan(testEntry(t, s, stateScriptOnly), scopeUser)
		err := s.install(context.Background(), plan)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("error = %v", err)
		}
		if entries, _ := os.ReadDir(s.userUnits); len(entries) != 0 {
			t.Errorf("left behind: %v", entries)
		}
		checkCalls(t, calls(), "systemctl --user show ", "systemd-analyze", "systemctl --user daemon-reload",
			"systemctl --user enable --now backup.timer", "systemctl --user disable --now backup.timer",
			"systemctl --user daemon-reload")
	})
}

func TestExecuteBit(t *testing.T) {
	custom := t.TempDir()
	writeFile(t, filepath.Join(custom, "service.tmpl"), "[Unit]\n[Service]\nExecStart={{.ExecStart}}\n", 0o644)
	direct := t.TempDir()
	writeFile(t, filepath.Join(direct, "service.tmpl"), "[Unit]\n[Service]\nExecStart=-{{.ScriptPath}} --run\n", 0o644)
	cases := map[string]struct {
		directory string
		mode      os.FileMode
	}{
		"interpreter prefix": {custom, 0o644},
		"script as command":  {direct, 0o744},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			defaultTools(t)
			s, _ := testSession(t)
			var err error
			if s.templates, err = loadTemplates([]string{test.directory}); err != nil {
				t.Fatal(err)
			}
			entry := testEntry(t, s, stateScriptOnly)
			if err := os.Chmod(entry.Script.Target, 0o644); err != nil {
				t.Fatal(err)
			}
			plan, _ := s.basePlan(entry, scopeUser)
			if err := s.install(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			if info, _ := os.Stat(entry.Script.Target); info.Mode().Perm() != test.mode {
				t.Errorf("script mode = %v, want %v", info.Mode().Perm(), test.mode)
			}
		})
	}
}

func TestUninstallFlow(t *testing.T) {
	t.Run("own unit", func(t *testing.T) {
		calls := defaultTools(t)
		s, _ := testSession(t)
		entry := testEntry(t, s, stateComplete)
		timerPath, servicePath := filepath.Join(s.userUnits, "backup.timer"), filepath.Join(s.userUnits, "backup.service")
		writeFile(t, timerPath, "x", 0o644)
		writeFile(t, servicePath, "x", 0o644)
		writeFile(t, filepath.Join(s.userUnits, "backup.timer.d", "override.conf"), "x", 0o644)
		entry.Timer = &Unit{Name: "backup.timer", LoadState: "loaded", FragmentPath: timerPath}
		entry.Service = &Unit{Name: "backup.service", LoadState: "loaded", FragmentPath: servicePath}
		if err := s.uninstall(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(s.userUnits)
		if len(entries) != 1 || entries[0].Name() != "backup.service" {
			t.Errorf("left = %v, want only the service", entries)
		}
		if _, err := os.Stat(entry.Script.Target); err != nil {
			t.Error("script was touched")
		}
		checkCalls(t, calls(), "systemctl --user disable --now backup.timer",
			"systemctl --user daemon-reload", "systemctl --user reset-failed backup.timer")
	})
	t.Run("package unit", func(t *testing.T) {
		calls := defaultTools(t)
		s, _ := testSession(t)
		entry := testEntry(t, s, stateTimerOnly)
		writeFile(t, filepath.Join(s.userUnits, "backup.timer"), "x", 0o644)
		entry.Timer = &Unit{Name: "backup.timer", LoadState: "loaded", FragmentPath: "/usr/lib/systemd/user/backup.timer"}
		if err := s.uninstall(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(s.userUnits, "backup.timer")); err != nil {
			t.Error("file removed although systemd reported a package unit")
		}
		checkCalls(t, calls(), "systemctl --user disable --now backup.timer",
			"systemctl --user daemon-reload", "systemctl --user reset-failed backup.timer")
	})
}

func TestDeletableFragment(t *testing.T) {
	cases := map[string]bool{
		"/etc/systemd/system/backup.timer":           true,
		"/etc/systemd/system/../system/backup.timer": true,
		"/etc/systemd/system/sub/backup.timer":       false,
		"/etc/systemd/system/../../tmp/backup.timer": false,
		"/usr/lib/systemd/system/backup.timer":       false,
		"/etc/systemd/system/other.timer":            false,
		"etc/systemd/system/backup.timer":            false,
		"":                                           false,
	}
	for fragment, want := range cases {
		if got := deletableFragment(fragment, systemUnitDirectory, "backup.timer"); got != want {
			t.Errorf("%q = %v, want %v", fragment, got, want)
		}
	}
}

func TestDryRun(t *testing.T) {
	calls := defaultTools(t)
	s, output := testSession(t)
	s.dryRun = true
	entry := testEntry(t, s, stateScriptOnly)
	if err := os.Chmod(entry.Script.Target, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, _ := s.basePlan(entry, scopeUser)
	if err := s.install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	timerPath := filepath.Join(s.userUnits, "backup.timer")
	entry.Timer = &Unit{Name: "backup.timer", LoadState: "loaded", FragmentPath: timerPath}
	writeFile(t, timerPath, "x", 0o644)
	if err := s.uninstall(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(timerPath); err != nil {
		t.Error("dry run removed the unit")
	}
	if _, err := os.Stat(filepath.Join(s.userUnits, "backup.service")); err == nil {
		t.Error("dry run wrote a unit")
	}
	if info, _ := os.Stat(entry.Script.Target); info.Mode().Perm() != 0o644 {
		t.Error("dry run changed the script")
	}
	checkCalls(t, calls(), "systemctl --user show ", "systemd-analyze verify")
	if !strings.Contains(output.String(), "Would run: systemctl --user enable --now backup.timer") {
		t.Errorf("dry run output misses the planned command:\n%s", output)
	}
}
