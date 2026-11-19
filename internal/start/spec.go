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
	"regexp"
)

// LaunchSpec is the request body (§4.1). Unknown fields are refused so a typo can't weaken it.
type LaunchSpec struct {
	V           int             `json:"v"`
	RequestID   string          `json:"request_id"`
	Owner       string          `json:"owner"`
	Machine     string          `json:"machine"`
	Label       string          `json:"label"`
	Realm       string          `json:"realm"`
	Resume      *Resume         `json:"resume"`
	Harness     string          `json:"harness"`
	HarnessArgs []string        `json:"harness_args"`
	Workdir     Workdir         `json:"workdir"`
	Profile     string          `json:"profile"`
	Billing     string          `json:"billing"`
	MsgScope    json.RawMessage `json:"msg_scope"`
	Client      json.RawMessage `json:"client"`
	Brief       *Brief          `json:"brief"`
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
	BadSpec            = "bad_spec"
	WrongMachine       = "wrong_machine"
	NotBuilt           = "not_built"
)

// Refusal is a policy refusal, not a failure.
type Refusal struct {
	Code string `json:"refused"`
	Msg  string `json:"message"`
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Msg }

func refuse(code, format string, a ...any) *Refusal {
	return &Refusal{Code: code, Msg: fmt.Sprintf(format, a...)}
}

var (
	labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_./-]{0,79}$`)
	ridRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.-]{0,99}$`)
	sidRe   = regexp.MustCompile(`^s[0-9a-z]{6,16}$`)
	userRe  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

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
	return &s, nil
}
