// Shared CLI frame: help, options, subcommands, colored output, signals, exit codes.
// Copy unchanged; the app supplies projectName, help, options, commands, and execute.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// ANSI colors of the shared output language
const (
	red    = "31"
	green  = "32"
	yellow = "33"
	cyan   = "36"
)

// Colored line output
type printer struct {
	out         io.Writer
	color       bool
	interactive bool
}

// Framed startup banner; a single line outside an interactive terminal
func (p printer) header() {
	if !p.interactive {
		p.line("🚀", projectName+" — starting", "")
		return
	}
	// Rocket is two cells wide everywhere, unlike the gear
	title := "  🚀  " + projectName + " — starting  "
	border := strings.Repeat("─", utf8.RuneCountInString(title)+1)
	fmt.Fprintln(p.out)
	for _, line := range []string{"╭" + border + "╮", "│" + title + "│", "╰" + border + "╯"} {
		p.print(line, cyan)
	}
}

func (p printer) line(icon, text, color string) {
	p.print(icon+" "+text, color)
}

// Categorized error line; returns the failure exit code
func (p printer) fail(category string, err error) int {
	p.line("❌", fmt.Sprintf("%s error: %v", category, err), red)
	return 1
}

func (p printer) print(text, color string) {
	if p.color && color != "" {
		text = "\033[" + color + "m" + text + "\033[0m"
	}
	fmt.Fprintln(p.out, text)
}

// Shared color of menu frames and progress bars: position of last, blended from pink through gold to turquoise
func tint(position, last int) string {
	percent := 100 * max(0, min(position, last)) / max(last, 1)
	from, to := [3]int{255, 45, 145}, [3]int{255, 190, 65}
	if percent > 50 {
		percent -= 50
		from, to = to, [3]int{45, 220, 195}
	}
	mix := min(percent*2, 100)
	return fmt.Sprintf("\033[38;2;%d;%d;%dm",
		from[0]+(to[0]-from[0])*mix/100,
		from[1]+(to[1]-from[1])*mix/100,
		from[2]+(to[2]-from[2])*mix/100)
}

// Parsed command line: option values by name ("" for flags), then positional arguments
type commandLine struct {
	options     map[string]string
	positionals []string
}

// Subcommand work after banner and configuration; gets the options and the arguments after its name
type command func(ctx context.Context, out printer, config Config, arguments commandLine) int

// Help, argument check, colors, signals, exit code
func run(arguments []string) int {
	if (len(arguments) > 0 && arguments[0] == "help") || slices.Contains(arguments, "-h") || slices.Contains(arguments, "--help") {
		fmt.Print(help)
		return 0
	}
	// Options are checked before the banner
	parsed, problem := parseCommandLine(arguments, options())
	if problem != "" {
		return usageFailure(problem)
	}
	// Default run takes options only; a positional names a subcommand
	work := func(ctx context.Context, out printer, config Config) int {
		return execute(ctx, out, config, parsed.options)
	}
	if len(parsed.positionals) > 0 {
		handler, ok := commands()[parsed.positionals[0]]
		if !ok {
			return usageError(parsed.positionals[0])
		}
		work = func(ctx context.Context, out printer, config Config) int {
			return handler(ctx, out, config, commandLine{parsed.options, parsed.positionals[1:]})
		}
	}

	// Logs, pipes, and services get quiet output without redraws
	_, noColor := os.LookupEnv("NO_COLOR")
	terminal := term.IsTerminal(int(os.Stdout.Fd()))
	interactive := terminal && term.IsTerminal(int(os.Stdin.Fd())) && os.Getenv("TERM") != "dumb"
	out := printer{os.Stdout, terminal && !noColor, interactive}

	// First Ctrl-C cancels cleanly, second one kills
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	context.AfterFunc(ctx, stop)

	code := start(ctx, out, work)
	if ctx.Err() != nil {
		out.line("🛑", "Interrupted", yellow)
		return 130
	}
	return code
}

// Split options from positional arguments; known maps each option to whether it takes a value.
// Returns the usage problem, empty on success.
func parseCommandLine(arguments []string, known map[string]bool) (commandLine, string) {
	parsed := commandLine{options: map[string]string{}}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		// Everything after -- is positional
		if argument == "--" {
			parsed.positionals = append(parsed.positionals, arguments[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			parsed.positionals = append(parsed.positionals, argument)
			continue
		}
		// --name, --name value, or --name=value; the last one wins
		name, value, hasValue := strings.Cut(argument, "=")
		takesValue, ok := known[name]
		switch {
		case !ok:
			return commandLine{}, "Unknown argument: " + argument
		case !takesValue && hasValue:
			return commandLine{}, "Option takes no value: " + name
		case takesValue && !hasValue:
			if index+1 == len(arguments) {
				return commandLine{}, "Missing value for " + name
			}
			index++
			value = arguments[index]
		}
		parsed.options[name] = value
	}
	return parsed, ""
}

// Usage problem on stderr; returns the failure exit code
func usageFailure(problem string) int {
	fmt.Fprintf(os.Stderr, "❌ %s\n", problem)
	fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", projectName)
	return 1
}

// Unknown argument on stderr; returns the failure exit code
func usageError(argument string) int {
	return usageFailure("Unknown argument: " + argument)
}

// Banner and configuration, then the default run or the named subcommand
func start(ctx context.Context, out printer, work func(context.Context, printer, Config) int) int {
	out.header()
	binaryDir, err := executableDir()
	if err != nil {
		return out.fail("Configuration", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out.fail("Configuration", err)
	}
	_, config, err := loadConfig(binaryDir, home)
	var created *configCreatedError
	if errors.As(err, &created) {
		out.line("📝", "Created configuration: "+created.path, yellow)
		out.line("✏️", "Replace every "+placeholder+" value, then rerun", "")
		return 1
	}
	if err != nil {
		return out.fail("Configuration", err)
	}
	return work(ctx, out, config)
}
