// Package start is the daemon side of `ccsbx start` (U2, build plan §4.1).
//
// The daemon mints sid and gen. The caller supplies a label, a profile and a billing mode and
// gets a StartReply or a Refusal. In direct mode (tonight) the "daemon" is this process holding a
// file lock on the state directory; the pane runs in `tmux -L ccsbx` as the calling user.
package start

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/user"
	"regexp"
	"strconv"
	"strings"
)

// LaunchSpec is the request body (§4.1). Unknown fields are refused so a typo can't weaken it.
type LaunchSpec struct {
	V           int      `json:"v"`
	RequestID   string   `json:"request_id"`
	Owner       string   `json:"owner"`
	Machine     string   `json:"machine"`
	Label       string   `json:"label"`
	Realm       string   `json:"realm"`
	Resume      *Resume  `json:"resume"`
	Harness     string   `json:"harness"`
	HarnessArgs []string `json:"harness_args"`
	Workdir     Workdir  `json:"workdir"`
	// GitDir is the resolved git directory of the workdir ("" if it has none). Only helper mode takes
	// it: the CLIENT resolves it as the owner (hostd, as the service uid, cannot enter the home) and
	// hostd checks its shape; in direct mode the daemon resolves it itself and a spec carrying one is
	// refused (see Daemon.Start).
	GitDir   string          `json:"gitdir,omitempty"`
	Profile  string          `json:"profile"`
	Billing  string          `json:"billing"`
	Grants   []string        `json:"grants"` // the grant names the node's Grants: line asks for (decided at the doors, M1)
	MsgScope json.RawMessage `json:"msg_scope"`
	Client   json.RawMessage `json:"client"`
	Brief    *Brief          `json:"brief"`

	built bool // the daemon built the worktree for the repo form (never from the wire: json ignores it)
}

// Resume asks for generation+1 of an existing session (U7).
type Resume struct {
	Sid string `json:"sid"`
}

// Workdir is a host path tonight; the repo/base/branch form needs the git route (H1, later).
type Workdir struct {
	Path   string `json:"path"`
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Branch string `json:"branch"`
}

// Brief is the first prompt text.
type Brief struct {
	Text string `json:"text"`
}

// StartReply is the success body (§4.1).
type StartReply struct {
	V       int     `json:"v"`
	Sid     string  `json:"sid"`
	Gen     int     `json:"gen"`
	ID      string  `json:"id"`
	Backend string  `json:"backend"`
	Pin     Pin     `json:"pin"`
	Attach  Attach  `json:"attach"`
	Journal Journal `json:"journal"`
	NotEv   bool    `json:"not_evidence,omitempty"` // direct mode: invariant 14 waived
}

// Pin records what the session runs on.
type Pin struct {
	CC      string `json:"cc"`
	Image   string `json:"image"`
	Channel string `json:"channel"`
}

// Attach says how to reach the pane.
type Attach struct {
	Local  []string `json:"local"`
	Remote []string `json:"remote"`
}

// Journal is the event cursor at start.
type Journal struct {
	Epoch string `json:"epoch"`
	Seq   int    `json:"seq"`
}

// Refusal codes (§3 U2). All map to exit 2.
const (
	ProfileNotApproved = "profile_not_approved"
	RateCapped         = "rate_capped"
	BillingUnavailable = "billing_unavailable"
	MachineFull        = "machine_full"
	AlreadyRunning     = "already_running"
	SessionBusy        = "session_busy"
	BadSpec            = "bad_spec"
	WrongMachine       = "wrong_machine"
	NotBuilt           = "not_built"
	NotRunning         = "not_running"
	ImageNotPinned     = "image_not_pinned"
	PodConflict        = "pod_conflict"    // a pod is under the name that is not ours
	PodNotReady        = "pod_not_ready"   // the pod did not come up in time; it was deleted
	PodLoadFailed      = "pod_load_failed" // the sidecar could not be loaded; the pod was deleted
	// the remote start (M2c)
	GrantNotAllowed  = "grant_not_allowed"
	ResumeNotYours   = "resume_not_yours"
	StartInProgress  = "start_in_progress"
	PeerUnreachable  = "peer_unreachable"
	PeerSetMismatch  = "peer_set_mismatch"
	RequestIDReused  = "request_id_reused"
	Forbidden        = "forbidden"
	Internal         = "internal" // a failure on this machine that is not the caller's spec
	UnsupportedMedia = "unsupported_media_type"
)

// Refusal is a policy refusal, not a failure.
type Refusal struct {
	Code string `json:"refused"`
	Msg  string `json:"message"`
	// already_running carries where, so a client can reattach without a second call; start_in_progress carries
	// when to come back (seconds, also sent as Retry-After)
	Machine    string `json:"machine,omitempty"`
	Sid        string `json:"sid,omitempty"`
	State      string `json:"state,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Msg }

// CheckOwner refuses a spec whose owner is not the user the caller really is. The owner names the
// session in its id (the journal and token identity), so it comes from the socket peer's uid (through
// the passwd entry), never from the spec: otherwise anyone who can reach the socket could start a
// session under another owner's name. lookupID is user.LookupId in production.
func CheckOwner(uid int, owner string, lookupID func(string) (*user.User, error)) error {
	u, err := lookupID(strconv.Itoa(uid))
	if err != nil || u.Username == "" {
		return refuse(BadSpec, "cannot tell who uid %d is (no passwd entry): refusing to start a session for an unknown caller", uid)
	}
	if owner != u.Username {
		return refuse(BadSpec, "owner %q is not the caller (%s): a session is started under the caller's own name", owner, u.Username)
	}
	return nil
}

func refuse(code, format string, a ...any) *Refusal {
	return &Refusal{Code: code, Msg: fmt.Sprintf(format, a...)}
}

var (
	realmRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_./-]{0,79}$`)
	ridRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.-]{0,99}$`)
	sidRe   = regexp.MustCompile(`^s[0-9a-z]{6,16}$`)
	userRe  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

	// the repo form (docs/m2c-remote-start.md §4): validated now so a later git route cannot receive a bad value
	repoRe = regexp.MustCompile(`^github\.com/[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	refRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
)

// validRef: a base or branch name that is safe to hand to git as ONE argv element (after --end-of-options): no
// "..", no "//", no trailing "/" or ".lock" or ".".
func validRef(s string) bool {
	return refRe.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") &&
		!strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".lock") && !strings.HasSuffix(s, ".")
}

// ParseSpec decodes and shape-checks a LaunchSpec. It does not apply policy.
func ParseSpec(b []byte) (*LaunchSpec, error) {
	var s LaunchSpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, refuse(BadSpec, "%v", err)
	}
	switch {
	case s.V != 1:
		return nil, refuse(BadSpec, "v must be 1, got %d", s.V)
	case !labelRe.MatchString(s.Label):
		return nil, refuse(BadSpec, "label %q is not valid", s.Label)
	case !ridRe.MatchString(s.RequestID):
		return nil, refuse(BadSpec, "request_id %q is not valid", s.RequestID)
	case !userRe.MatchString(s.Owner):
		return nil, refuse(BadSpec, "owner %q is not valid", s.Owner)
	case s.Machine == "":
		return nil, refuse(BadSpec, "machine is required")
	case s.Harness != "claude":
		return nil, refuse(BadSpec, "harness must be claude, got %q", s.Harness)
	case s.Profile == "":
		return nil, refuse(BadSpec, "profile is required")
	}
	if w := s.Workdir; w.Repo != "" || w.Base != "" || w.Branch != "" {
		if !repoRe.MatchString(w.Repo) || !validRef(w.Base) || !validRef(w.Branch) {
			return nil, refuse(BadSpec, "workdir repo/base/branch: repo github.com/<owner>/<repo>, base and branch plain ref names")
		}
	}
	// One normalised realm everywhere (journal, verb queue, directory): an empty one is the
	// production realm, and anything the verb queue would refuse is refused here, at start.
	if s.Realm == "" {
		s.Realm = "production"
	}
	if !realmRe.MatchString(s.Realm) {
		return nil, refuse(BadSpec, "realm %q is not valid (lowercase letters, digits, dash)", s.Realm)
	}
	return &s, nil
}

// ValidSid reports whether s has the shape of a daemon-minted session id. Anything that names a
// file or a container by sid checks this first.
func ValidSid(s string) bool { return sidRe.MatchString(s) }
