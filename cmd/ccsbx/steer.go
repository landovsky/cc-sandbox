// session.prompt and session.replace (C9b, the daemon half): how cc-tree steers a FENCED session.
// The model of a fenced session lives in the daemon's container, so a nudge, a roll or a reseed
// can't fork its transcript or type into a host pane: it asks the daemon, over the 0600 socket.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

// exitSteerBusy: the session is working or at a menu; nothing was typed or replaced. exitNoSession:
// no live session for --label. (The numbers cc-tree's stub daemon uses.)
const (
	exitSteerBusy = 4
	exitNoSession = 3
)

func steerFlags(f *cli.Flags) {
	clientFlags(f)
	f.String("label", "", "the node the session works on (tree:id)")
	f.Bool("json", false, "print the reply as JSON")
}

// steerTarget resolves --label to the live session's sid.
func steerTarget(f *cli.Flags, a *api, name string, stderr io.Writer) (string, int) {
	label := f.Lookup("label").Value.String()
	if label == "" {
		fmt.Fprintf(stderr, "ccsbx %s: --label is required\n", name)
		return "", cli.ExitUsage
	}
	code, b, err := a.do("GET", "/v1/resolve?label="+url.QueryEscape(label), nil, nil)
	if err != nil {
		return "", fail(stderr, name, err)
	}
	if code == 404 {
		fmt.Fprintf(stderr, "ccsbx %s: no live session for %q\n", name, label)
		return "", exitNoSession
	}
	var r struct{ Sid string }
	if code != 200 || json.Unmarshal(b, &r) != nil || r.Sid == "" {
		return "", httpFail(stderr, name, code, b)
	}
	return r.Sid, 0
}

func promptCmd() *cli.Command {
	return &cli.Command{
		Name:    "prompt",
		Summary: "session.prompt: type steering text into a fenced session, only at its prompt (C9b)",
		Recipes: `  echo "please write a checkpoint now" | ccsbx prompt --label cc-sandbox:3cvt --kind nudge --json
  The text comes on stdin. It is typed (stamped 'trust: unfenced (owner device)') only when the
  session's pane is at its input prompt. Exit 0 typed, 3 no live session for --label,
  4 the session is working or at a menu: nothing was typed (retry later).`,
		Setup: func(f *cli.Flags) {
			steerFlags(f)
			f.String("kind", "message", "what the text is: message, nudge, checkpoint, ...")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			a := newAPI(f)
			text, _ := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
			if strings.TrimSpace(string(text)) == "" {
				fmt.Fprintln(stderr, "ccsbx prompt: the text is read from stdin and is empty")
				return cli.ExitUsage
			}
			sid, rc := steerTarget(f, a, "prompt", stderr)
			if rc != 0 {
				return rc
			}
			code, b, err := a.do("POST", "/v1/sessions/"+url.PathEscape(sid)+"/prompt",
				map[string]any{"kind": f.Lookup("kind").Value.String(), "text": string(text)}, nil)
			if err != nil {
				return fail(stderr, "prompt", err)
			}
			switch code {
			case 200:
				fmt.Fprintln(stdout, `{"ok":true}`)
				return cli.ExitOK
			case 409:
				fmt.Fprintln(stderr, "ccsbx prompt: the session is working or asking; nothing was typed")
				return exitSteerBusy
			case 404:
				return exitNoSession
			}
			return httpFail(stderr, "prompt", code, b)
		},
	}
}

func replaceCmd() *cli.Command {
	return &cli.Command{
		Name:    "replace",
		Summary: "session.replace: generation+1 of a fenced session with a new brief, at a turn boundary (C9b)",
		Recipes: `  ccsbx replace --label cc-sandbox:3cvt --json < brief.md      {"v":1,"sid":"s…","gen":2}
  The brief is read from stdin. Only when the session's turn is over (its pane is at the prompt):
  then the pane is ended and generation+1 starts with the brief: same sid, same node, workdir and
  profile, a fresh outbox. Exit 0 replaced, 3 no live session, 4 the session is mid-turn:
  nothing was touched. The hostd that serves the socket starts the new pane (--ccsbx-bin,
  --profiles, --image).`,
		Setup: steerFlags,
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			a := newAPI(f)
			brief, _ := io.ReadAll(io.LimitReader(os.Stdin, 200<<10))
			if strings.TrimSpace(string(brief)) == "" {
				fmt.Fprintln(stderr, "ccsbx replace: the brief is read from stdin and is empty")
				return cli.ExitUsage
			}
			sid, rc := steerTarget(f, a, "replace", stderr)
			if rc != 0 {
				return rc
			}
			code, b, err := a.do("POST", "/v1/sessions/"+url.PathEscape(sid)+"/replace", map[string]any{"brief": string(brief)}, nil)
			if err != nil {
				return fail(stderr, "replace", err)
			}
			switch code {
			case 200:
				fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
				return cli.ExitOK
			case 409:
				fmt.Fprintln(stderr, "ccsbx replace: the session is mid-turn; nothing was replaced")
				return exitSteerBusy
			case 404:
				return exitNoSession
			}
			return httpFail(stderr, "replace", code, b)
		},
	}
}
