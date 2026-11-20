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
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// SideBinary is the sidecar image's ccsbx, by absolute path: the image is FROM scratch, so there is no PATH
// to find it by, for the container's own command and for `kubectl exec ... ccsbx side load` alike.
const SideBinary = "/usr/local/bin/ccsbx"

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
	scope, err := canonicalScope(i.Scope)
	if err != nil {
		return nil, fmt.Errorf("sidecar: msg_scope: %v", err)
	}
	args := []string{SideBinary, "side", "serve",
		"--listen", "127.0.0.1:" + strconv.Itoa(SidePort),
		"--control", SideControl, "--state", SideState, "--journal-dir", SideJournalDir,
		"--out", SideOut, "--in", SideIn, "--fence-uid", strconv.Itoa(FenceUID),
		"--sid", i.Sid, "--gen", strconv.Itoa(i.Gen), "--label", i.Label, "--full-id", i.FullID,
		"--machine", i.Machine, "--scope", scope}
	if i.HubURL != "" {
		if err := checkHTTPURL(i.HubURL); err != nil {
			return nil, fmt.Errorf("sidecar: hub url: %v", err)
		}
		args = append(args, "--hub-url", i.HubURL)
	}
	names := map[string]bool{}
	for _, d := range i.Doors {
		if !doorRe.MatchString(d) {
			return nil, fmt.Errorf("sidecar: door %q must be NAME=URL", d)
		}
		name := d[:strings.Index(d, "=")]
		if names[name] {
			return nil, fmt.Errorf("sidecar: door %q is named twice", name)
		}
		names[name] = true
		if err := checkHTTPURL(d[strings.Index(d, "=")+1:]); err != nil {
			return nil, fmt.Errorf("sidecar: door %q: %v", d, err)
		}
		args = append(args, "--door", d)
	}
	return args, nil
}

// ScopeDoc is the message scope the sidecar understands (internal/msg.Scope). The sidecar's own parser
// is not the place to catch a duplicated or wrong-case key, so the launcher builds the flag from this
// struct: what is written is exactly these fields, in this order, once each.
type ScopeDoc struct {
	Manager      ScopeNode         `json:"manager"`
	Peers        []string          `json:"peers"`
	PeerMachines map[string]string `json:"peer_machines,omitempty"`
	ReplyWindowS int               `json:"reply_window_s,omitempty"`
}

type ScopeNode struct {
	Node    string `json:"node"`
	Machine string `json:"machine,omitempty"`
}

// canonicalScope refuses anything that is not a JSON object with exactly the known keys, each once and in
// its exact spelling (no duplicates, no "Manager", no extras, no null), and returns the compact JSON
// of the typed struct.
func canonicalScope(raw json.RawMessage) (string, error) {
	d, err := ParseScope(raw)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ParseScope reads a record's msg_scope as strictly as the sidecar's flag is built: a JSON object with
// exactly the known keys, spelled exactly, each once, nothing after it. Everything that reads the scope out of a
// record (the sidecar's flag, the relay's may_reach and manager) goes through it, so none of them sees a
// record the others would refuse.
func ParseScope(raw json.RawMessage) (*ScopeDoc, error) {
	if len(raw) == 0 || len(raw) > maxScope {
		return nil, fmt.Errorf("must be a JSON object of at most %d bytes", maxScope)
	}
	if err := strictKeys(raw, map[string]bool{"manager": true, "peers": true, "peer_machines": true, "reply_window_s": true}, false); err != nil {
		return nil, err
	}
	var d ScopeDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	// the manager object is held to the same rule
	var top map[string]json.RawMessage
	json.Unmarshal(raw, &top)
	if m, ok := top["manager"]; ok {
		if err := strictKeys(m, map[string]bool{"node": true, "machine": true}, false); err != nil {
			return nil, fmt.Errorf("manager: %v", err)
		}
	}
	if pm, ok := top["peer_machines"]; ok {
		if err := strictKeys(pm, nil, true); err != nil {
			return nil, fmt.Errorf("peer_machines: %v", err)
		}
	}
	if d.Peers == nil {
		d.Peers = []string{}
	}
	return &d, nil
}

// strictKeys walks one JSON object's top-level keys: each must be exactly spelled, listed (when free is
// false) and not repeated. A value that is not an object (null included) is refused.
func strictKeys(raw json.RawMessage, allowed map[string]bool, free bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return fmt.Errorf("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		k, _ := kt.(string)
		if seen[k] {
			return fmt.Errorf("key %q appears twice", k)
		}
		seen[k] = true
		if !free && !allowed[k] {
			return fmt.Errorf("key %q is not one the sidecar knows (keys are exact and lower-case)", k)
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("data after the object")
	}
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
