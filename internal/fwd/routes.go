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
	{Door: "egress", Methods: []string{"GET"}, Path: "/github/**"},
	{Door: "egress", Methods: []string{"POST"}, Path: "/github/repos/*/*/pulls"},
	{Door: "egress", Methods: []string{"POST"}, Path: "/github/repos/*/*/issues/*/comments"},
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

// CleanPath returns the path a door would see, or false if the path could be read
// two ways. fwd forwards exactly the checked bytes, so there is no second parse
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
	if path.Clean(raw) != raw && !(strings.HasSuffix(raw, "/") && path.Clean(raw)+"/" == raw) {
		return "", false
	}
	if strings.Contains(raw, "//") {
		return "", false
	}
	return raw, true
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
