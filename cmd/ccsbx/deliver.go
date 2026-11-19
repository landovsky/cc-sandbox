package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/msg"
)

func deliverCmd() *cli.Command {
	return &cli.Command{
		Name:    "deliver",
		Summary: "owner-device delivery into a session's pane, stamped 'unfenced (owner device)' (U3b)",
		Recipes: `  ccsbx deliver --to cc-sandbox:2f3e --from cc-tree "ping"     # by node (the live session now)
  ccsbx deliver --to s0k1dry66qg --text -  < note.txt              # by sid, text from stdin
  The stamp is "[MSG from <from> · trust: unfenced (owner device)] <text>": never 'human'.
  No scope rule (the owner may write to any live session). The text is flattened to one line.
  Exit 0 delivered, 1 failed, 2 no live session for --to.`,
		Setup: func(f *cli.Flags) {
			stateFlags(f)
			f.String("to", "", "target: a node (label), bus id or sid")
			f.String("from", "owner", "who the stamp names")
			f.String("text", "", "message text; - reads stdin (or give it as arguments)")
			f.String("tmux-socket", "ccsbx", "tmux -L socket the panes live on")
			f.Bool("json", false, "print the receipt as JSON")
		},
		Run: runDeliver,
	}
}

func runDeliver(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	text := get("text")
	if text == "-" {
		b, _ := io.ReadAll(os.Stdin)
		text = string(b)
	}
	if text == "" {
		text = strings.Join(args, " ")
	}
	if get("to") == "" || strings.TrimSpace(text) == "" {
		fmt.Fprintln(stderr, "ccsbx deliver: --to and a text are required")
		return cli.ExitUsage
	}
	d, err := daemonFrom(f)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx deliver:", err)
		return cli.ExitFail
	}
	tm := &msg.Tmux{Socket: get("tmux-socket")}
	rt := &msg.Router{Dir: msg.DaemonDir{D: d, Alive: tm.Alive}, Out: tm, StateDir: d.StateDir}
	res := rt.DeliverOwner(get("from"), get("to"), text)
	if get("json") == "true" {
		b, _ := json.Marshal(res)
		fmt.Fprintln(stdout, string(b))
	}
	switch {
	case res.Status == "ok":
		if get("json") != "true" {
			fmt.Fprintf(stdout, "delivered to %s (receipt %s)\n", res.To, res.Receipt)
		}
		return cli.ExitOK
	case res.Code == "no_live_session":
		fmt.Fprintf(stderr, "ccsbx deliver: no live session for %q\n", get("to"))
		return cli.ExitRefused
	}
	fmt.Fprintf(stderr, "ccsbx deliver: %s %s\n", res.Status, res.Code)
	return cli.ExitFail
}
