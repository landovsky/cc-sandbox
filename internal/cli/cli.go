// Package cli is a tiny subcommand layer over the standard flag package.
//
// Rules it enforces for every command (code is read and run by the next agent):
//   - every flag has a CCSBX_<NAME> environment variable, shown in --help;
//   - --help works on every subcommand and never runs anything;
//   - recipes live in the command's Recipes text, so --help is the manual.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Exit codes shared by all subcommands.
const (
	ExitOK      = 0
	ExitFail    = 1
	ExitRefused = 2 // a refusal by policy (no billing, not approved, ...)
	ExitUsage   = 64
)

// Command is one subcommand.
type Command struct {
	Name    string
	Summary string
	Recipes string // examples, printed under the flags in --help
	Setup   func(fs *Flags)
	// Raw hands every argument after the command name to Run untouched: no flag parsing and no
	// generic --help. For commands with their own richer flag set and --help (which must still
	// print a "Flags:" line and run nothing).
	Raw bool
	// Run gets the parsed flags and the remaining args.
	Run func(fs *Flags, args []string, stdout, stderr io.Writer) int
}

// Flags wraps flag.FlagSet and gives every flag an env-var default.
type Flags struct {
	*flag.FlagSet
	// Getenv is os.Getenv in production; tests replace it.
	Getenv func(string) string
}

// EnvName is the environment variable behind a flag name: "key-file" -> CCSBX_KEY_FILE.
func EnvName(flagName string) string {
	return "CCSBX_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func note(name, usage string) string { return usage + " (env " + EnvName(name) + ")" }

// String declares a string flag whose default is $CCSBX_<NAME> if set, else def.
func (f *Flags) String(name, def, usage string) *string {
	if v := f.Getenv(EnvName(name)); v != "" {
		def = v
	}
	return f.FlagSet.String(name, def, note(name, usage))
}

// Bool declares a bool flag; env values 1/true/yes turn it on.
func (f *Flags) Bool(name string, def bool, usage string) *bool {
	if v := f.Getenv(EnvName(name)); v != "" {
		def = v == "1" || v == "true" || v == "yes"
	}
	return f.FlagSet.Bool(name, def, note(name, usage))
}

// Int declares an int flag with an env default; a malformed env value is ignored.
func (f *Flags) Int(name string, def int, usage string) *int {
	if v := f.Getenv(EnvName(name)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			def = n
		}
	}
	return f.FlagSet.Int(name, def, note(name, usage))
}

// App is the command table plus program identity.
type App struct {
	Name    string
	Summary string
	Version string
	Cmds    []*Command
	Getenv  func(string) string
}

func (a *App) find(name string) *Command {
	for _, c := range a.Cmds {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Main runs the app and returns the exit code.
func (a *App) Main(args []string, stdout, stderr io.Writer) int {
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		a.usage(stdout)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Fprintf(stdout, "%s %s\n", a.Name, a.Version)
		return ExitOK
	}
	c := a.find(args[0])
	if c == nil {
		fmt.Fprintf(stderr, "%s: unknown command %q (try %s --help)\n", a.Name, args[0], a.Name)
		return ExitUsage
	}
	fs := &Flags{FlagSet: flag.NewFlagSet(a.Name+" "+c.Name, flag.ContinueOnError), Getenv: a.Getenv}
	fs.SetOutput(stderr)
	if c.Raw {
		return c.Run(fs, args[1:], stdout, stderr)
	}
	if c.Setup != nil {
		c.Setup(fs)
	}
	fs.Usage = func() {}
	// --help prints to stdout and exits 0 before Run is ever reached.
	for _, x := range args[1:] {
		if x == "--" {
			break
		}
		if x == "-h" || x == "--help" || x == "-help" {
			a.cmdUsage(stdout, c, fs)
			return ExitOK
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintf(stderr, "%s %s: %v\n", a.Name, c.Name, err)
		return ExitUsage
	}
	return c.Run(fs, fs.Args(), stdout, stderr)
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintf(w, "%s — %s\n\nUsage:\n  %s <command> [flags]\n  %s <command> --help\n\nCommands:\n", a.Name, a.Summary, a.Name, a.Name)
	cmds := append([]*Command(nil), a.Cmds...)
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintf(w, "  %-14s %s\n", "version", "print the build version")
	fmt.Fprintf(w, "\nEvery flag also reads CCSBX_<FLAG> (dashes become underscores).\n")
}

func (a *App) cmdUsage(w io.Writer, c *Command, fs *Flags) {
	fmt.Fprintf(w, "%s %s — %s\n\nFlags:\n", a.Name, c.Name, c.Summary)
	fs.SetOutput(w)
	fs.PrintDefaults()
	if c.Recipes != "" {
		fmt.Fprintf(w, "\nRecipes:\n%s\n", strings.TrimRight(c.Recipes, "\n"))
	}
}
