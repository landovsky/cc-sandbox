// Tests for the flag, env and help layer. They guard against a --help that starts the command, and
// against help text that lacks the flags and env variables a next agent needs to run it.

package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func newApp(run func(f *Flags) int) *App {
	return &App{Name: "t", Summary: "x", Version: "v0",
		Getenv: func(k string) string {
			if k == "CCSBX_LABEL" {
				return "from-env"
			}
			return ""
		},
		Cmds: []*Command{{
			Name: "run", Summary: "runs a thing", Recipes: "  t run --label x",
			Setup: func(f *Flags) { f.String("label", "", "the label") },
			Run:   func(f *Flags, _ []string, _, _ io.Writer) int { return run(f) },
		}}}
}

func TestCLI(t *testing.T) {
	t.Run("a look-around with --help must not start anything", func(t *testing.T) {
		ran := false
		a := newApp(func(*Flags) int { ran = true; return 0 })
		var out, errb bytes.Buffer
		if code := a.Main([]string{"run", "--help"}, &out, &errb); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if ran {
			t.Fatal("--help ran the command")
		}
		for _, want := range []string{"CCSBX_LABEL", "Recipes:", "t run --label x"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("help lacks %q:\n%s", want, out.String())
			}
		}
	})

	t.Run("every tunable is reachable without code: env is the default, a flag wins", func(t *testing.T) {
		var got string
		a := newApp(func(f *Flags) int { got = f.Lookup("label").Value.String(); return 0 })
		var o, e bytes.Buffer
		a.Main([]string{"run"}, &o, &e)
		if got != "from-env" {
			t.Errorf("env default: got %q", got)
		}
		a.Main([]string{"run", "--label", "flag"}, &o, &e)
		if got != "flag" {
			t.Errorf("flag override: got %q", got)
		}
	})

	t.Run("an unknown command is a usage error, not a silent success", func(t *testing.T) {
		a := newApp(func(*Flags) int { return 0 })
		var o, e bytes.Buffer
		if code := a.Main([]string{"nope"}, &o, &e); code != ExitUsage {
			t.Fatalf("exit %d", code)
		}
	})

	t.Run("env names follow the flag names", func(t *testing.T) {
		if EnvName("key-file") != "CCSBX_KEY_FILE" {
			t.Fatal(EnvName("key-file"))
		}
	})
}

// context: cc-tree calls `ccsbx cursor get NAME --json`: flags after the positionals. A command
// that opts in takes them; one that doesn't (run -- cmd args) keeps everything after the first
// positional for itself.
func TestInterleavedFlagsOnlyForCommandsThatAskForThem(t *testing.T) {
	var got []string
	var jsonSet string
	mk := func(inter bool) *App {
		return &App{Name: "x", Cmds: []*Command{{Name: "c", Interleave: inter,
			Setup: func(f *Flags) { f.Bool("json", false, "j") },
			Run: func(f *Flags, args []string, _, _ io.Writer) int {
				got, jsonSet = args, f.Lookup("json").Value.String()
				return 0
			}}}, Getenv: func(string) string { return "" }}
	}
	var out, errb bytes.Buffer
	mk(true).Main([]string{"c", "get", "NAME", "--json"}, &out, &errb)
	if strings.Join(got, ",") != "get,NAME" || jsonSet != "true" {
		t.Fatalf("interleaved: %v json=%s", got, jsonSet)
	}
	mk(false).Main([]string{"c", "get", "NAME", "--json"}, &out, &errb)
	if strings.Join(got, ",") != "get,NAME,--json" || jsonSet != "false" {
		t.Fatalf("plain: %v json=%s", got, jsonSet)
	}
}
