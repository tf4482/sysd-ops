//go:build linux

// sysd-ops links shell scripts to systemd timers and services.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

const projectName = "sysd-ops"

// Version number from the VERSION file
//
//go:embed VERSION
var versionFile string

const help = projectName + ` — link shell scripts to systemd timers and services

Usage:
  ` + projectName + ` [options]                  Show the scripts; pick one and an action in a terminal
  ` + projectName + ` list [--json] [--verbose]  Show the scripts and their units
  ` + projectName + ` install <name> [options]   Create and enable the missing service and timer
  ` + projectName + ` edit-timer <name>          Edit <name>.timer with systemctl edit
  ` + projectName + ` edit-service <name>        Edit <name>.service with systemctl edit
  ` + projectName + ` uninstall <name> [--yes]   Disable and remove <name>.timer, optionally the service
  ` + projectName + ` --version
  ` + projectName + ` --help | -h | help

Options:
  --dir <path>              Script directory for this run
  --recursive               Search subdirectories too
  --system                  Show system units only
  --user                    Show user units only
  --all                     Show system and user units
  --template-dir <path>     Directory with service.tmpl and timer.tmpl
  --dry-run                 Show commands and file contents, change nothing
  --yes                     Answer confirmations with yes
  --no-color                Print without colors
  --version                 Show the version
  --json                    list: print JSON
  --verbose                 list: show unit files, drop-ins, and templates
  --dropin                  edit-*: write a drop-in override instead of the full unit
  --scope <scope>           install: user or system
  --schedule <preset>       install: default, hourly, daily@HH:MM, weekly@Mon@HH:MM,
                            monthly, or oncalendar:<expression>
  --on-boot <span>          install: OnBootSec=, e.g. 60s
  --every-inactive <span>   install: OnUnitInactiveSec=, interval after a run finished
  --every-active <span>     install: OnUnitActiveSec=, interval after a run started
  --user-account <name>     install: User= of a system service (default: current user)

Configuration:
  Uses the first existing file without merging:
  1. config.yml beside the executable
  2. ~/.config/` + projectName + `/config.yml
  The working directory is not searched.
  With no file, creates a user-only template at location 2 and exits 1.
  Templates: --template-dir, settings.template_dir, $XDG_CONFIG_HOME/` + projectName + `/templates,
  then the built-in ones.

Behavior:
  Finds .sh files and shell files without extension and matches each name exactly
  to <name>.timer and <name>.service in the user and system scope.
  Installs never overwrite units; uninstalls delete only files in /etc/systemd/system
  or ~/.config/systemd/user. Scripts are never changed, except chmod u+x on request.

Exit codes:
  0    Everything succeeded or nothing was changed on request
  1    Configuration, usage, systemd, or runtime error
  130  Interrupted by the user
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// Known options: name -> takes a value
func options() map[string]bool {
	return map[string]bool{
		"--dir": true, "--recursive": false, "--system": false, "--user": false, "--all": false,
		"--template-dir": true, "--dry-run": false, "--yes": false, "--no-color": false,
		"--version": false, "--json": false, "--verbose": false, "--dropin": false,
		"--scope": true, "--schedule": true, "--on-boot": true, "--every-inactive": true,
		"--every-active": true, "--user-account": true,
	}
}

// Subcommands besides the default run
func commands() map[string]command {
	return map[string]command{
		"list":         listCommand,
		"install":      installCommand,
		"edit-timer":   editCommand(kindTimer),
		"edit-service": editCommand(kindService),
		"uninstall":    uninstallCommand,
	}
}

// Default run: the menu in a terminal, the table elsewhere
func execute(ctx context.Context, out printer, config Config, options map[string]string) int {
	if _, ok := options["--version"]; ok {
		out.line("🏷️", projectName+" "+version(), "")
		return 0
	}
	s, code := prepare(out, config, options, true)
	if s == nil {
		return code
	}
	if out.interactive {
		return s.browse(ctx)
	}
	return s.list(ctx, false, false)
}

// Version without surrounding whitespace
func version() string {
	return strings.TrimSpace(versionFile)
}

// list subcommand
func listCommand(ctx context.Context, out printer, config Config, arguments commandLine) int {
	if len(arguments.positionals) > 0 {
		return usageError(arguments.positionals[0])
	}
	_, asJSON := arguments.options["--json"]
	_, verbose := arguments.options["--verbose"]
	s, code := prepare(out, config, arguments.options, !asJSON)
	if s == nil {
		return code
	}
	return s.list(ctx, asJSON, verbose)
}

// install subcommand: values from options, defaults for the rest
func installCommand(ctx context.Context, out printer, config Config, arguments commandLine) int {
	s, name, code := prepareNamed(out, config, arguments)
	if s == nil {
		return code
	}
	entry, err := s.findEntry(ctx, name, func(entry Entry) bool { return entry.State != stateComplete })
	if err != nil {
		return out.fail("Selection", err)
	}
	if scope, ok := arguments.options["--scope"]; ok && entry.Scope != "" && entry.Scope != strings.ToLower(scope) {
		return out.fail("Selection", fmt.Errorf("%s has units in %s scope; install there or omit --scope", name, entry.Scope))
	}
	plan, err := s.planFromOptions(ctx, entry, arguments.options)
	if err != nil {
		return out.fail("Input", err)
	}
	return s.report("Install", s.install(ctx, plan))
}

// edit-timer and edit-service subcommands
func editCommand(kind string) command {
	return func(ctx context.Context, out printer, config Config, arguments commandLine) int {
		s, name, code := prepareNamed(out, config, arguments)
		if s == nil {
			return code
		}
		entry, err := s.findEntry(ctx, name, func(entry Entry) bool { return entryUnit(entry, kind).exists() })
		if err != nil {
			return out.fail("Selection", err)
		}
		_, dropin := arguments.options["--dropin"]
		return s.report("Edit", s.edit(ctx, entry.Scope, name+"."+kind, dropin))
	}
}

// uninstall subcommand
func uninstallCommand(ctx context.Context, out printer, config Config, arguments commandLine) int {
	s, name, code := prepareNamed(out, config, arguments)
	if s == nil {
		return code
	}
	entry, err := s.findEntry(ctx, name, func(entry Entry) bool { return entry.Timer.exists() })
	if err != nil {
		return out.fail("Selection", err)
	}
	return s.report("Uninstall", s.uninstall(ctx, entry))
}

// Timer or service of an entry
func entryUnit(entry Entry, kind string) *Unit {
	if kind == kindTimer {
		return entry.Timer
	}
	return entry.Service
}

// Session for subcommands taking exactly one script name
func prepareNamed(out printer, config Config, arguments commandLine) (*session, string, int) {
	switch len(arguments.positionals) {
	case 0:
		return nil, "", usageFailure("Missing script name")
	case 1:
	default:
		return nil, "", usageError(arguments.positionals[1])
	}
	s, code := prepare(out, config, arguments.options, true)
	return s, arguments.positionals[0], code
}

// Session from configuration and options; nil with the exit code on failure
func prepare(out printer, config Config, options map[string]string, announce bool) (*session, int) {
	if _, ok := options["--no-color"]; ok {
		out.color = false
	}
	s, err := newSession(out, config, options)
	var usage *invalidUsage
	if errors.As(err, &usage) {
		return nil, usageFailure(usage.problem)
	}
	if err != nil {
		return nil, out.fail("Configuration", err)
	}
	if announce {
		out.line("📂", "Scripts: "+s.directory+" · from "+s.directorySource, "")
		_, verbose := options["--verbose"]
		if out.interactive || verbose {
			out.line("🧩", "Templates: "+s.templateSources(), "")
		}
	}
	return s, 0
}

// Usage problem found after parsing, such as conflicting options
type invalidUsage struct{ problem string }

func (e *invalidUsage) Error() string { return e.problem }

// Resolve directory, scopes, templates, and the current account
func newSession(out printer, config Config, options map[string]string) (*session, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	workdir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	account, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("cannot read the current user: %v", err)
	}
	s := &session{
		out:         out,
		config:      config,
		userUnits:   filepath.Join(home, ".config", "systemd", "user"),
		systemUnits: systemUnitDirectory,
		account:     account.Username,
	}
	_, s.recursive = options["--recursive"]
	s.recursive = s.recursive || config.Recursive
	_, s.dryRun = options["--dry-run"]
	_, s.yes = options["--yes"]

	s.directory, s.directorySource = workdir, "working directory"
	if value, ok := options["--dir"]; ok {
		s.directory, s.directorySource = expandPath(value, home, workdir), "flag"
	} else if config.Directory != "" {
		s.directory, s.directorySource = expandPath(config.Directory, home, workdir), "config"
	}
	if info, err := os.Stat(s.directory); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("script directory %s not found", s.directory)
	}

	scope := config.Scope
	var chosen []string
	for _, name := range []string{scopeSystem, scopeUser, scopeAll} {
		if _, ok := options["--"+name]; ok {
			chosen = append(chosen, "--"+name)
			scope = name
		}
	}
	if len(chosen) > 1 {
		return nil, &invalidUsage{"Use only one of " + strings.Join(chosen, ", ")}
	}
	s.scopes = map[string][]string{scopeUser: {scopeUser}, scopeSystem: {scopeSystem}, scopeAll: {scopeUser, scopeSystem}}[scope]

	var directories []string
	explicit := config.TemplateDir
	if value, ok := options["--template-dir"]; ok {
		explicit = value
	}
	if explicit != "" {
		explicit = expandPath(explicit, home, workdir)
		if info, err := os.Stat(explicit); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("template directory %s not found", explicit)
		}
		directories = append(directories, explicit)
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	directories = append(directories, filepath.Join(configHome, projectName, "templates"))
	if s.templates, err = loadTemplates(directories); err != nil {
		return nil, err
	}
	return s, nil
}
