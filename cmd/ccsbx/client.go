package main

// The daemon's client commands: what cc-tree (the cockpit) calls on a machine's daemon. Each one
// is a thin wrapper over the daemon's unix socket (<state>/hostd.sock); none of them touches the
// fence or the state directory itself. `ccsbx <command> --help` has the recipes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

// exitHeld: the cockpit lease is held by someone else, or ours was lost.
const exitHeld = 4

func unixClient(sock string) *http.Client {
	return &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

// api is one daemon.
type api struct {
	c     *http.Client
	sock  string
	state string
}

func newAPI(f *cli.Flags) *api {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	sock := get("socket")
	if sock == "" {
		sock = filepath.Join(get("state"), "hostd.sock")
	}
	return &api{c: unixClient(sock), sock: sock, state: get("state")}
}

func clientFlags(f *cli.Flags) {
	home, _ := os.UserHomeDir()
	f.String("state", filepath.Join(home, ".local", "state", "ccsbx"), "daemon state directory")
	f.String("socket", "", "the daemon's unix socket (default <state>/hostd.sock)")
}

// do sends one request and returns status and body.
func (a *api) do(method, path string, body any, hdr map[string]string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://d"+path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("the daemon isn't answering on %s (is `ccsbx-hostd run` up?): %v", a.sock, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return resp.StatusCode, b, nil
}

func fail(stderr io.Writer, name string, err error) int {
	fmt.Fprintf(stderr, "ccsbx %s: %v\n", name, err)
	return cli.ExitFail
}

func httpFail(stderr io.Writer, name string, code int, body []byte) int {
	fmt.Fprintf(stderr, "ccsbx %s: HTTP %d %s\n", name, code, strings.TrimSpace(string(body)))
	return cli.ExitFail
}

// ---- ls ----

func lsCmd() *cli.Command {
	return &cli.Command{
		Name:    "ls",
		Summary: "the sessions the daemon runs now",
		Recipes: `  ccsbx ls                 one line per running session: <label>/g<gen> <sid>
  ccsbx ls --json          [{"sid","label","gen","realm","state":"running","idle"}]`,
		Setup: func(f *cli.Flags) { clientFlags(f); f.Bool("json", false, "print the JSON array") },
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			a := newAPI(f)
			code, b, err := a.do("GET", "/v1/sessions", nil, nil)
			if err != nil {
				return fail(stderr, "ls", err)
			}
			if code != 200 {
				return httpFail(stderr, "ls", code, b)
			}
			if f.Lookup("json").Value.String() == "true" {
				fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
				return cli.ExitOK
			}
			var rows []struct {
				Sid, Label string
				Gen        int
			}
			_ = json.Unmarshal(b, &rows)
			for _, r := range rows {
				fmt.Fprintf(stdout, "%s/g%d %s\n", r.Label, r.Gen, r.Sid)
			}
			return cli.ExitOK
		},
	}
}

// ---- events ----

func eventsCmd() *cli.Command {
	return &cli.Command{
		Name:    "events",
		Summary: "replay the journal after a cursor (what the cockpit's follower reads)",
		Recipes: `  ccsbx events --after 41 --json                      {"epoch","head","events":[...]}
  ccsbx events --after 41 --epoch E --wait 20 --json  long-poll up to 20 s
  A journal of another epoch answers {"epoch": <new>, "events": []}: start again from 0.
  A cursor older than retention answers the oldest retained events with "gap": true.`,
		Setup: func(f *cli.Flags) {
			clientFlags(f)
			f.Int("after", 0, "return events with a seq greater than this")
			f.String("epoch", "", "the epoch the caller remembers (empty: any)")
			f.Int("wait", 0, "seconds to wait for a new event")
			f.String("realm", "", "only this realm's events")
			f.Int("limit", 0, "at most this many events (0: all)")
			f.Bool("json", true, "(always JSON)")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			a := newAPI(f)
			q := url.Values{"after": {get("after")}}
			for _, k := range []string{"epoch", "wait", "realm", "limit"} {
				if v := get(k); v != "" && v != "0" {
					q.Set(k, v)
				}
			}
			code, b, err := a.do("GET", "/v1/events?"+q.Encode(), nil, nil)
			if err != nil {
				return fail(stderr, "events", err)
			}
			var e struct {
				Error  string `json:"error"`
				Epoch  string `json:"epoch"`
				Oldest int64  `json:"oldest"`
			}
			_ = json.Unmarshal(b, &e)
			switch code {
			case 200:
				fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
				return cli.ExitOK
			case 409: // the journal was replaced: the caller compares epochs and starts again
				fmt.Fprintf(stdout, "{\"epoch\":%q,\"events\":[]}\n", e.Epoch)
				return cli.ExitOK
			case 410: // retention cut the caller's cursor: give what is left, and say so
				q.Set("after", fmt.Sprint(e.Oldest-1))
				q.Del("epoch")
				code, b, err = a.do("GET", "/v1/events?"+q.Encode(), nil, nil)
				if err != nil || code != 200 {
					return httpFail(stderr, "events", code, b)
				}
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				m["gap"] = true
				out, _ := json.Marshal(m)
				fmt.Fprintln(stdout, string(out))
				return cli.ExitOK
			}
			return httpFail(stderr, "events", code, b)
		},
	}
}

// ---- cursor ----

func cursorCmd() *cli.Command {
	return &cli.Command{
		Name:       "cursor",
		Interleave: true,
		Summary:    "get or set a named journal cursor (kept at the daemon)",
		Recipes: `  ccsbx cursor get cockpit-hp-ubuntu --json                         {"seq","epoch","tree_sha"} ({"seq":0} if never set)
  ccsbx cursor set cockpit-hp-ubuntu --seq 41 --epoch E --tree-sha <sha>
  ccsbx cursor list                                                 {"epoch","head","cursors":[...]} newest first
  A write cursor moves only with the SHA of the tree commit its events produced (or the tree's
  HEAD when they changed nothing); a read cursor (name starts with tab-) needs none.`,
		Setup: func(f *cli.Flags) {
			clientFlags(f)
			f.Int("seq", 0, "set: the last seq applied")
			f.String("epoch", "", "set: the journal's epoch")
			f.String("tree-sha", "", "set: the tree commit the events produced")
			f.Bool("json", true, "(always JSON)")
		},
		Run: func(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			if len(args) == 1 && args[0] == "list" {
				code, b, err := newAPI(f).do("GET", "/v1/cursors", nil, nil)
				if err != nil {
					return fail(stderr, "cursor", err)
				}
				if code != 200 {
					return httpFail(stderr, "cursor", code, b)
				}
				fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
				return cli.ExitOK
			}
			if len(args) != 2 || (args[0] != "get" && args[0] != "set") {
				fmt.Fprintln(stderr, "usage: ccsbx cursor list | cursor get NAME | cursor set NAME --seq N --epoch E [--tree-sha SHA]")
				return cli.ExitUsage
			}
			a := newAPI(f)
			path := "/v1/cursors/" + url.PathEscape(args[1])
			if args[0] == "get" {
				code, b, err := a.do("GET", path, nil, nil)
				if err != nil {
					return fail(stderr, "cursor", err)
				}
				if code == 404 {
					fmt.Fprintln(stdout, `{"seq":0}`)
					return cli.ExitOK
				}
				if code != 200 {
					return httpFail(stderr, "cursor", code, b)
				}
				fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
				return cli.ExitOK
			}
			var seq int64
			fmt.Sscanf(get("seq"), "%d", &seq)
			code, b, err := a.do("POST", path, map[string]any{"seq": seq, "epoch": get("epoch"), "tree_sha": get("tree-sha")}, nil)
			if err != nil {
				return fail(stderr, "cursor", err)
			}
			if code != 200 {
				return httpFail(stderr, "cursor", code, b)
			}
			fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
			return cli.ExitOK
		},
	}
}

// ---- pending ----

func pendingCmd() *cli.Command {
	return &cli.Command{
		Name:    "pending",
		Summary: "the cockpit requests still queued for a node (read-only, no lease needed)",
		Recipes: `  ccsbx pending --node cc-sandbox:3cvt --type ccf.checkpoint --json     [{id,seq,sid,type,body,...}]`,
		Setup: func(f *cli.Flags) {
			clientFlags(f)
			f.String("realm", "production", "the realm's queue")
			f.String("node", "", "only this node's requests")
			f.String("type", "", "only this request type, e.g. ccf.checkpoint")
			f.Bool("json", true, "(always JSON)")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			a := newAPI(f)
			q := url.Values{"realm": {get("realm")}, "node": {get("node")}, "type": {get("type")}}
			code, b, err := a.do("GET", "/v1/cockpit/pending?"+q.Encode(), nil, nil)
			if err != nil {
				return fail(stderr, "pending", err)
			}
			if code != 200 {
				return httpFail(stderr, "pending", code, b)
			}
			fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
			return cli.ExitOK
		},
	}
}

// ---- cockpit ----

type heldLease struct {
	Realm   string    `json:"realm"`
	Holder  string    `json:"holder"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

func leasePath(state, realm string) string { return filepath.Join(state, "cockpit", realm+".lease") }

func loadLease(state, realm string) (heldLease, bool) {
	var l heldLease
	b, err := os.ReadFile(leasePath(state, realm))
	if err != nil || json.Unmarshal(b, &l) != nil {
		return l, false
	}
	return l, true
}

func saveLease(state string, l heldLease) error {
	p := leasePath(state, l.Realm)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(l)
	return os.WriteFile(p, b, 0o600)
}

func cockpitCmd() *cli.Command {
	return &cli.Command{
		Name:       "cockpit",
		Interleave: true,
		Summary:    "the cockpit lease and verb results: up | down | status | requests | result",
		Recipes: `  ccsbx cockpit up [--realm production] [--holder hp-ubuntu/cc-tree] [--steal]
        take (or renew) the realm's lease; the token is kept in <state>/cockpit/<realm>.lease.
        Exit 4 if another holder has it (they are named), unless --steal.
  ccsbx cockpit status [--realm R]     depth, the oldest wait, the ⚠ items, who holds the lease
  ccsbx cockpit requests [--realm R]   the pending requests (the holder only; one per session)
  ccsbx cockpit result <id> --status ok|refused|error|stale|transient --text T [--commit SHA]
        report what applying request <id> did; renews or takes the lease first. A transient
        result leaves the request queued (⚠); a permanent one goes to the session's inbox.
  ccsbx cockpit down [--realm R]       give the lease up
  Lease TTL is the daemon's (--lease-ttl, 90 s); call up/result at least that often.`,
		Setup: func(f *cli.Flags) {
			clientFlags(f)
			f.String("realm", "production", "production | e2e")
			host, _ := os.Hostname()
			f.String("holder", host+"/cc-tree", "who holds the lease (named in a refusal)")
			f.Bool("steal", false, "take the lease from a live holder (owner operation)")
			f.String("status", "ok", "result: ok|refused|error|stale|transient")
			f.String("text", "", "result: what happened, in a line")
			f.String("commit", "", "result: the tree commit it produced")
			f.Int("limit", 0, "requests: at most this many")
			f.Bool("json", true, "(always JSON)")
		},
		Run: runCockpit,
	}
}

func (a *api) acquire(realm, holder string, steal bool) (heldLease, int, []byte, error) {
	cur, _ := loadLease(a.state, realm)
	code, b, err := a.do("POST", "/v1/cockpit/lease?realm="+url.QueryEscape(realm),
		map[string]any{"holder": holder, "token": cur.Token, "steal": steal}, nil)
	if err != nil || code != 200 {
		return heldLease{}, code, b, err
	}
	var l heldLease
	_ = json.Unmarshal(b, &l)
	l.Realm = realm
	return l, code, b, saveLease(a.state, l)
}

func runCockpit(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: ccsbx cockpit up|down|status|requests|result … (--help)")
		return cli.ExitUsage
	}
	a, realm := newAPI(f), get("realm")
	held := func(code int, b []byte) int {
		fmt.Fprintf(stderr, "ccsbx cockpit: the %s lease: %s\n", realm, strings.TrimSpace(string(b)))
		return exitHeld
	}
	switch args[0] {
	case "up":
		l, code, b, err := a.acquire(realm, get("holder"), get("steal") == "true")
		if err != nil {
			return fail(stderr, "cockpit", err)
		}
		if code == 409 {
			fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
			return held(code, b)
		}
		if code != 200 {
			return httpFail(stderr, "cockpit", code, b)
		}
		out, _ := json.Marshal(map[string]any{"realm": realm, "holder": l.Holder, "expires": l.Expires})
		fmt.Fprintln(stdout, string(out))
		return cli.ExitOK
	case "down":
		cur, ok := loadLease(a.state, realm)
		if !ok {
			fmt.Fprintln(stdout, `{"released":false,"note":"no lease held here"}`)
			return cli.ExitOK
		}
		code, b, err := a.do("POST", "/v1/cockpit/lease?realm="+url.QueryEscape(realm)+"&release=1", map[string]any{"token": cur.Token}, nil)
		if err != nil {
			return fail(stderr, "cockpit", err)
		}
		_ = os.Remove(leasePath(a.state, realm))
		if code != 200 && code != 409 { // 409: it had already expired or been taken: nothing to release
			return httpFail(stderr, "cockpit", code, b)
		}
		fmt.Fprintln(stdout, `{"released":true}`)
		return cli.ExitOK
	case "status":
		code, b, err := a.do("GET", "/v1/cockpit/status?realm="+url.QueryEscape(realm), nil, nil)
		if err != nil {
			return fail(stderr, "cockpit", err)
		}
		if code != 200 {
			return httpFail(stderr, "cockpit", code, b)
		}
		fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
		return cli.ExitOK
	case "requests":
		cur, ok := loadLease(a.state, realm)
		if !ok {
			fmt.Fprintln(stderr, "ccsbx cockpit: no lease held here (ccsbx cockpit up)")
			return exitHeld
		}
		path := "/v1/cockpit/requests?realm=" + url.QueryEscape(realm)
		if get("limit") != "0" {
			path += "&limit=" + get("limit")
		}
		code, b, err := a.do("GET", path, nil, map[string]string{"X-Lease": cur.Token})
		if err != nil {
			return fail(stderr, "cockpit", err)
		}
		if code == 409 {
			return held(code, b)
		}
		if code != 200 {
			return httpFail(stderr, "cockpit", code, b)
		}
		fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
		return cli.ExitOK
	case "result":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: ccsbx cockpit result <id> --status S --text T [--commit SHA]")
			return cli.ExitUsage
		}
		// A result is a cockpit act: take or renew the lease first (never steal).
		l, code, b, err := a.acquire(realm, get("holder"), false)
		if err != nil {
			return fail(stderr, "cockpit", err)
		}
		if code == 409 {
			return held(code, b)
		}
		if code != 200 {
			return httpFail(stderr, "cockpit", code, b)
		}
		text, _ := json.Marshal(get("text"))
		code, b, err = a.do("POST", "/v1/cockpit/requests/"+url.PathEscape(args[1])+"/result?realm="+url.QueryEscape(realm),
			map[string]any{"status": get("status"), "code": "", "result": json.RawMessage(text), "commit": get("commit")},
			map[string]string{"X-Lease": l.Token})
		if err != nil {
			return fail(stderr, "cockpit", err)
		}
		if code == 409 {
			return held(code, b)
		}
		if code != 200 {
			return httpFail(stderr, "cockpit", code, b)
		}
		fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
		return cli.ExitOK
	}
	fmt.Fprintf(stderr, "ccsbx cockpit: unknown subcommand %q\n", args[0])
	return cli.ExitUsage
}
