// routes.go is the forwarder's per-door method and path allowlist. It exists so the fence can reach
// only what a door is meant to serve, with decision and admin paths never routable (default deny).

package fwd

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Rule allows one method set on one path pattern of one door.
//
// Pattern syntax: segments split on "/". "*" matches exactly one segment, a
// trailing "**" matches one or more remaining segments, anything else is literal.
type Rule struct {
	Door    string   `json:"door"`
	Methods []string `json:"methods"`
	Path    string   `json:"path"`
}

// DefaultRules is the built-in allowlist: what a fenced Claude Code session needs
// from the model door, and the egress door's GitHub reads and the two writes plan
// U15 allows. Everything else is refused (default deny). Replace it with --routes.
var DefaultRules = []Rule{
	{Door: "model", Methods: []string{"POST"}, Path: "/v1/messages"},
	{Door: "model", Methods: []string{"POST"}, Path: "/v1/messages/count_tokens"},
	{Door: "model", Methods: []string{"GET"}, Path: "/v1/models"},
	{Door: "model", Methods: []string{"GET"}, Path: "/v1/models/*"},
	// The scripted stub (C18) is its own door with its own audience, so a stub token never verifies at
	// the real model door; its routes are the model's.
	{Door: "stub", Methods: []string{"POST"}, Path: "/v1/messages"},
	{Door: "stub", Methods: []string{"POST"}, Path: "/v1/messages/count_tokens"},
	{Door: "stub", Methods: []string{"GET"}, Path: "/v1/models"},
	{Door: "stub", Methods: []string{"GET"}, Path: "/v1/models/*"},
	{Door: "egress", Methods: []string{"GET"}, Path: "/github/**"},
	{Door: "egress", Methods: []string{"POST"}, Path: "/github/repos/*/*/pulls"},
	{Door: "egress", Methods: []string{"POST"}, Path: "/github/repos/*/*/issues/*/comments"},
}

// GitHeader carries the sidecar's own capability for the git routes. Only the sidecar's git sends it, and the
// forwarder strips every x-ccsbx-* header before the door sees the request.
const GitHeader = "X-Ccsbx-Git"

// GitRules are the git smart-HTTP routes of the egress door (F3c), owner/<repo>.git as two segments. They are NOT in
// DefaultRules: the forwarder the fence reaches must never route git, because it injects the token for any caller
// and a fence that could POST git-receive-pack would bypass every check the sidecar makes. They apply only to a
// request that carries the sidecar's secret (Config.GitSecret), and to nothing else.
var GitRules = []Rule{
	{Door: "egress", Methods: []string{"GET"}, Path: "/git/*/*/info/refs"},
	{Door: "egress", Methods: []string{"POST"}, Path: "/git/*/*/git-upload-pack"},
	{Door: "egress", Methods: []string{"POST"}, Path: "/git/*/*/git-receive-pack"},
}

// denySegments are path segments that make a route a decision, approval or
// owner endpoint. They are refused before the allowlist is consulted, so even
// a careless "**" rule can never route them (T:approve-via-fwd). P7: decisions
// belong to the door's owner listener, never to a session token.
var denySegments = map[string]bool{
	"approve": true, "approvals": true, "approval": true, "decide": true,
	"decision": true, "decisions": true, "grants": true, "owner": true,
	"admin": true, "revoke": true, "keys": true, "sign": true,
}

// Verdict of a route check.
type Verdict int

const (
	Allow        Verdict = iota
	DenyPath             // not a clean path (traversal, encodings, empty segments)
	DenyDecision         // a decision/owner endpoint: never routable
	DenyNoRule           // default deny
)

func (v Verdict) String() string {
	return [...]string{"allow", "unclean path", "decision endpoint", "no matching rule"}[v]
}

// CleanPath returns the escaped path a door would see, or false if the path could be read
// two ways. fwd forwards exactly these bytes (as RawPath), so there is no second parse
// that could disagree with this one.
func CleanPath(u *url.URL) (string, bool) {
	raw := u.EscapedPath()
	if raw == "" || raw[0] != '/' {
		return "", false
	}
	// Any percent-encoding of a separator or dot, or a backslash, is refused:
	// doors may decode it differently from us.
	low := strings.ToLower(raw)
	for _, bad := range []string{"%2f", "%5c", "%2e", "%00", "\\", ";"} {
		if strings.Contains(low, bad) {
			return "", false
		}
	}
	for _, r := range raw {
		if r < 0x21 || r == 0x7f {
			return "", false
		}
	}
	// Decode once and require the escaped form to be the canonical encoding of the decoded path.
	// An escape that only hides a plain character (%61 for "a") is then refused, so the checks below
	// see the same segments the door will after it decodes, and a double encoding (the decoded path
	// still holds a "%") is refused too.
	dec, err := url.PathUnescape(raw)
	if err != nil || strings.Contains(dec, "%") || (&url.URL{Path: dec}).EscapedPath() != raw {
		return "", false
	}
	if path.Clean(raw) != raw && !(strings.HasSuffix(raw, "/") && path.Clean(raw)+"/" == raw) {
		return "", false
	}
	if strings.Contains(raw, "//") {
		return "", false
	}
	return raw, true
}

// QueryHasDecisionWord reports whether a query string of a non-read request carries a decision
// word as a whole token in a key or value (?action=approve). Decisions live on the owner
// listener, so this is defence in depth behind a broad --routes file.
func QueryHasDecisionWord(rawQuery string) bool {
	// Doors decode the query, so test every layer of decoding (up to a few), not the raw bytes only.
	// A query that does not decode at all is treated as carrying one.
	q := rawQuery
	for i := 0; i < 4; i++ {
		for _, tok := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			if denySegments[tok] {
				return true
			}
		}
		d, err := url.QueryUnescape(q)
		if err != nil {
			return true
		}
		if d == q {
			return false
		}
		q = d
	}
	return true
}

// Check decides whether method+p (a clean path, without query) may go to door.
func Check(rules []Rule, door, method, p string) Verdict {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return DenyPath
		}
		if denySegments[strings.ToLower(s)] {
			return DenyDecision
		}
		if d, err := url.PathUnescape(s); err != nil || denySegments[strings.ToLower(d)] {
			return DenyDecision // defence in depth: CleanPath already refuses encoded plain letters
		}
	}
	for _, r := range rules {
		if r.Door != door || !hasMethod(r.Methods, method) {
			continue
		}
		if match(strings.Split(strings.Trim(r.Path, "/"), "/"), segs) {
			return Allow
		}
	}
	return DenyNoRule
}

func hasMethod(ms []string, m string) bool {
	for _, x := range ms {
		if strings.EqualFold(x, m) {
			return true
		}
	}
	return false
}

func match(pat, segs []string) bool {
	for i, p := range pat {
		if p == "**" {
			return i == len(pat)-1 && len(segs) > i
		}
		if i >= len(segs) || (p != "*" && p != segs[i]) {
			return false
		}
	}
	return len(pat) == len(segs)
}

// ValidateRules refuses a rules file that could route a decision endpoint, or
// names an unknown door, so a bad file fails at start, not at the first call.
func ValidateRules(rules []Rule, doors map[string]*url.URL) error {
	for i, r := range rules {
		if _, ok := doors[r.Door]; !ok {
			return fmt.Errorf("rule %d: unknown door %q", i, r.Door)
		}
		if len(r.Methods) == 0 || r.Path == "" || r.Path[0] != '/' {
			return fmt.Errorf("rule %d: needs methods and an absolute path", i)
		}
		if first := strings.SplitN(strings.Trim(r.Path, "/"), "/", 2)[0]; strings.EqualFold(first, "git") {
			return fmt.Errorf("rule %d: %q would route git, which only the sidecar's own git may reach", i, r.Path)
		}
		for _, m := range r.Methods {
			if m == "*" || strings.EqualFold(m, "CONNECT") {
				return fmt.Errorf("rule %d: method %q not allowed", i, m)
			}
		}
		for _, s := range strings.Split(strings.Trim(r.Path, "/"), "/") {
			if denySegments[strings.ToLower(s)] {
				return fmt.Errorf("rule %d: %q is a decision endpoint and can never be routed", i, r.Path)
			}
		}
	}
	return nil
}
