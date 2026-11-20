// `ccsbx deliver`: the owner's device sends text into a live session through the daemon (U3b).
// It exists so cc-tree never types into a fenced pane itself: the daemon checks the pane is at its
// prompt and stamps the message as unfenced (owner device), never as a human.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

// exitBusy: the target is not at its input prompt, so nothing was typed (retry, or --queue).
const exitBusy = 3

func deliverCmd() *cli.Command {
	return &cli.Command{
		Name:    "deliver",
		Summary: "owner-device delivery into a session's pane, through the daemon, stamped 'unfenced (owner device)' (U3b)",
		Recipes: `  ccsbx deliver --to cc-sandbox:2f3e --from cc-tree "ping"     # by node (the live session now)
  ccsbx deliver --to s0k1dry66qg --text -  < note.txt              # by sid, text from stdin
  ccsbx deliver --to cc-sandbox:2f3e --queue "when you are free"   # wait for the prompt instead of failing
  The stamp is "[MSG from <from> · trust: unfenced (owner device) · #<boundary>] <text> ‹end #<boundary>›":
  never 'human'. <from> is a label ([A-Za-z0-9:._/-], other characters become _). The text is
  flattened to one line. No scope rule (the owner may write to any live session), but the idle rule
  holds: text is typed only into a pane at its input prompt (the session's last hook event is
  Stop); a pane in a turn or at a menu is never typed into.
  Talks to the daemon on <state>/hostd.sock (ccsbx-hostd run). Exit 0 delivered or queued, 1 failed,
  2 no live session for --to, 3 target busy (not typed).`,
		Setup: func(f *cli.Flags) {
			home, _ := os.UserHomeDir()
			f.String("state", filepath.Join(home, ".local", "state", "ccsbx"), "daemon state directory")
			f.String("socket", "", "the daemon's unix socket (default <state>/hostd.sock)")
			f.String("to", "", "target: a node (label), or a sid")
			f.String("from", "owner", "who the stamp names (a label)")
			f.String("text", "", "message text; - reads stdin (or give it as arguments)")
			f.Bool("queue", false, "if the target is busy, hold the message until its prompt is back (up to the daemon's TTL)")
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
	to := get("to")
	if to == "" || strings.TrimSpace(text) == "" {
		fmt.Fprintln(stderr, "ccsbx deliver: --to and a text are required")
		return cli.ExitUsage
	}
	sock := get("socket")
	if sock == "" {
		sock = filepath.Join(get("state"), "hostd.sock")
	}
	c := unixClient(sock)
	// A node (label) is resolved to its live session by the daemon; a sid is used as it is.
	sid := to
	if strings.Contains(to, ":") {
		resp, err := c.Get("http://d/v1/resolve?label=" + url.QueryEscape(to))
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx deliver: the daemon isn't answering on %s (is `ccsbx-hostd run` up?): %v\n", sock, err)
			return cli.ExitFail
		}
		var r struct {
			Sid string `json:"sid"`
		}
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if resp.StatusCode == 404 {
			fmt.Fprintf(stderr, "ccsbx deliver: no live session for %q\n", to)
			return cli.ExitRefused
		}
		if resp.StatusCode != 200 || r.Sid == "" {
			fmt.Fprintf(stderr, "ccsbx deliver: resolve failed (HTTP %d)\n", resp.StatusCode)
			return cli.ExitFail
		}
		sid = r.Sid
	}
	body, _ := json.Marshal(map[string]any{"from": get("from"), "text": text, "queue": get("queue") == "true"})
	resp, err := c.Post("http://d/v1/sessions/"+url.PathEscape(sid)+"/deliver", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "ccsbx deliver: the daemon isn't answering on %s: %v\n", sock, err)
		return cli.ExitFail
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if get("json") == "true" {
		fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
	}
	var res struct {
		Status, Code, Receipt, To string
	}
	json.Unmarshal(raw, &res)
	switch resp.StatusCode {
	case 200:
		if get("json") != "true" {
			fmt.Fprintf(stdout, "delivered to %s (receipt %s)\n", res.To, res.Receipt)
		}
		return cli.ExitOK
	case 202:
		if get("json") != "true" {
			fmt.Fprintf(stdout, "queued for %s: it waits for the prompt (receipt %s)\n", to, res.Receipt)
		}
		return cli.ExitOK
	case 404:
		fmt.Fprintf(stderr, "ccsbx deliver: no live session for %q\n", to)
		return cli.ExitRefused
	case 409:
		fmt.Fprintf(stderr, "ccsbx deliver: %s is busy (not at its input prompt); nothing was typed. Retry, or use --queue\n", to)
		return exitBusy
	}
	fmt.Fprintf(stderr, "ccsbx deliver: HTTP %d %s %s\n", resp.StatusCode, res.Status, res.Code)
	return cli.ExitFail
}
