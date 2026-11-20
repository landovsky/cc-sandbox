// starthelper.go is `ccsbx start` in helper mode: the spec goes to ccsbx-hostd over its local
// socket and hostd, running as the service uid, mints the session, writes the record where
// ccsbx-root reads it (/var/lib/ccsbx) and launches the pane. It exists because a record written
// by the caller under its own home is one ccsbx-root rightly refuses, so a session started that
// way would be reported as started and then die at once. Nothing is written by this process.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// defaultHostdSocket is where the installer's hostd unit puts its local socket.
const defaultHostdSocket = "/run/ccsbx-api/hostd.sock"

func startViaHostd(f *cli.Flags, spec []byte, asJSON bool, stdout, stderr io.Writer) int {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	if get("launcher") != "tmux" || get("run-cmd") != "" {
		fmt.Fprintln(stderr, "ccsbx start: --launcher and --run-cmd are direct-mode dev flags: in helper mode hostd decides the launcher and the command")
		return cli.ExitUsage
	}
	ls, err := start.ParseSpec(spec)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitRefused
	}
	// hostd (another uid) cannot enter the owner's home: resolve the workdir and the gitdir HERE, as the
	// owner, and send the resolved paths; hostd checks their shape and ccsbx-root validates them for real.
	if ls.GitDir != "" {
		fmt.Fprintln(stderr, "ccsbx start: the spec must not carry a gitdir: the client computes it")
		return cli.ExitRefused
	}
	if err := start.ResolveClientPaths(ls, os.Getuid()); err != nil {
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitRefused
	}
	spec, err = json.Marshal(ls)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitFail
	}
	sock := get("socket")
	c := unixClient(sock)
	resp, err := c.Post("http://hostd/v1/start", "application/json", bytes.NewReader(spec))
	if err != nil {
		if isDialError(err) {
			fmt.Fprintf(stderr, "ccsbx start: ccsbx-hostd is not running or not reachable (socket %s): %v\n  In helper mode sessions are started by hostd; this tool does not start them itself.\n", sock, err)
			return cli.ExitRefused
		}
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitFail
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		var rep start.StartReply
		if err := json.Unmarshal(body, &rep); err != nil {
			fmt.Fprintln(stderr, "ccsbx start: hostd's reply is not a StartReply:", err)
			return cli.ExitFail
		}
		if asJSON {
			b, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Fprintln(stdout, string(b))
		} else {
			fmt.Fprintf(stdout, "started %s (%s) attach: %v\n", rep.Sid, rep.ID, rep.Attach.Local)
		}
		return cli.ExitOK
	case http.StatusConflict:
		var r start.Refusal
		if json.Unmarshal(body, &r) != nil || r.Code == "" {
			fmt.Fprintf(stderr, "ccsbx start: hostd refused with an unreadable body: %s\n", strings.TrimSpace(string(body)))
			return cli.ExitRefused
		}
		if asJSON {
			fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
		}
		fmt.Fprintf(stderr, "ccsbx start: refused: %s\n", &r)
		return cli.ExitRefused
	default:
		fmt.Fprintf(stderr, "ccsbx start: hostd answered %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return cli.ExitFail
	}
}

// isDialError: the request never reached a daemon (no socket, refused, permission denied).
func isDialError(err error) bool {
	for e := error(err); e != nil; {
		if op, ok := e.(*net.OpError); ok && op.Op == "dial" {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
