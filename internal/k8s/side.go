// side.go builds the sidecar's command line (`ccsbx side serve ...`) for a session pod. The sidecar takes
// its identity and its message scope from flags in the pod spec, so this is the one place those are made,
// and every value comes from the daemon's session record, root's config or a constant here: nothing the
// fence can write. Every value is checked again here because it ends up in the pod spec, which the cluster
// (and anyone who can `get` the pod) reads. No secret goes in: the token, the generation key and the relay
// token reach the sidecar in the one exec load, in memory only.
//
// The paths are the sidecar's (internal/side): volume roots are root-owned 0777, so every directory the
// sidecar uses is made BY serve one level below the mount, 0700 and its own.
package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Where the sidecar keeps things: the control socket and ledger under the side-state volume, the journal
// under its own sidecar-only volume, and the outbox (read only) and inbox (read-write) it shares with the fence.
const (
	SideControl    = "/run/ccsbx/side/ctl/ctl.sock"
	SideState      = "/run/ccsbx/side/state"
	SideJournalDir = "/run/ccsbx/journal/s"
	SideOut        = "/run/ccsbx/out"
	SideIn         = "/run/ccsbx/in"
)

// SideInfo is what the sidecar is told about its session.
type SideInfo struct {
	Sid, Label, FullID, Machine string
	Gen                         int
	Scope                       json.RawMessage // the record's msg_scope: {manager:{node,machine},peers,peer_machines,...}
	HubURL                      string          // the operator egress Service to the hub; "" until it exists
	Doors                       []string        // NAME=URL, the doors the forwarder may reach
}

var (
	fullIDRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}/[A-Za-z0-9._-]{1,64}/s[0-9a-z]{6,16}/g[0-9]{1,4}$`)
	machineRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	doorRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}=`)
)

const maxScope = 8 << 10

// SideArgv is the sidecar container's command.
func SideArgv(i SideInfo) ([]string, error) {
	if !sidRe.MatchString(i.Sid) || i.Gen < 1 || i.Gen > 9999 {
		return nil, fmt.Errorf("sidecar: sid %q gen %d", i.Sid, i.Gen)
	}
	if len(i.Label) == 0 || len(i.Label) > 200 || strings.IndexFunc(i.Label, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return nil, fmt.Errorf("sidecar: label must be 1..200 printable characters")
	}
	if !fullIDRe.MatchString(i.FullID) || !strings.HasSuffix(i.FullID, "/"+i.Sid+"/g"+strconv.Itoa(i.Gen)) {
		return nil, fmt.Errorf("sidecar: full id %q is not <owner>/<machine>/%s/g%d", i.FullID, i.Sid, i.Gen)
	}
	if !machineRe.MatchString(i.Machine) {
		return nil, fmt.Errorf("sidecar: machine %q", i.Machine)
	}
	var obj map[string]json.RawMessage
	if len(i.Scope) == 0 || len(i.Scope) > maxScope || json.Unmarshal(i.Scope, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("sidecar: msg_scope must be a JSON object of at most %d bytes", maxScope)
	}
	var scope strings.Builder
	if err := compact(&scope, i.Scope); err != nil {
		return nil, fmt.Errorf("sidecar: msg_scope: %v", err)
	}
	if strings.ContainsRune(scope.String(), 0) {
		return nil, fmt.Errorf("sidecar: msg_scope holds a NUL")
	}
	args := []string{"ccsbx", "side", "serve",
		"--listen", "127.0.0.1:" + strconv.Itoa(SidePort),
		"--control", SideControl, "--state", SideState, "--journal-dir", SideJournalDir,
		"--out", SideOut, "--in", SideIn, "--fence-uid", strconv.Itoa(FenceUID),
		"--sid", i.Sid, "--gen", strconv.Itoa(i.Gen), "--label", i.Label, "--full-id", i.FullID,
		"--machine", i.Machine, "--scope", scope.String()}
	if i.HubURL != "" {
		if err := checkHTTPURL(i.HubURL); err != nil {
			return nil, fmt.Errorf("sidecar: hub url: %v", err)
		}
		args = append(args, "--hub-url", i.HubURL)
	}
	for _, d := range i.Doors {
		if !doorRe.MatchString(d) {
			return nil, fmt.Errorf("sidecar: door %q must be NAME=URL", d)
		}
		if err := checkHTTPURL(d[strings.Index(d, "=")+1:]); err != nil {
			return nil, fmt.Errorf("sidecar: door %q: %v", d, err)
		}
		args = append(args, "--door", d)
	}
	return args, nil
}

func compact(b *strings.Builder, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return err
	}
	b.Write(buf.Bytes())
	return nil
}

// checkHTTPURL: http or https, a host (and port), no credentials, path, query or fragment.
func checkHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, " \t\n\x00") {
		return fmt.Errorf("%q must be http(s)://host[:port] with no credentials, path or query", s)
	}
	return nil
}
