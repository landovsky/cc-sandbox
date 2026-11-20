// Package remotestart is the daemon side of M2c: a session started on THIS machine because the owner asked
// from another one (POST /v1/sessions on the tailnet listener). The contract is docs/m2c-remote-start.md; this
// is what keeps it: who may ask (the handler: owner class only), what a remote caller may ask for (grants and
// harness flags are the PROFILE's to allow, never the request's), how often (3 a minute per started_by, 10 a
// minute per calling machine, an hourly budget apart from the local one), that a lost answer can be retried
// without a second session (request_id, keyed per caller), and that one label never has two live generations
// across machines (a reservation on every machine, asked of every listed peer, with no tie-break).
//
// It decides nothing about HOW a session starts: that stays start.Daemon.Start, called through Starter, so
// the workdir root check, the profile, pins and caps are exactly the local start's.
package remotestart

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// Caller is who the tailnet says is asking, stamped by the daemon (never read from the request).
type Caller struct {
	StartedBy string // "owner:<login>@<device>"
	Machine   string // the calling device (rate-limited as a unit)
}

// Peer is another machine's daemon: its name on the hub and the base URL of its tailnet listener.
type Peer struct{ Name, URL string }

// View is one live session of a label as a daemon reports it.
type View struct {
	Machine string `json:"machine"`
	Sid     string `json:"sid"`
	Gen     int    `json:"gen"`
	State   string `json:"state"`
}

// Reservation is a daemon saying "a start of this label is in progress here".
type Reservation struct {
	Machine string `json:"machine"`
	TS      int64  `json:"ts"`
}

// Answer is what GET /v1/resolve?v=2 returns.
type Answer struct {
	Live     []View       `json:"live"`
	Reserved *Reservation `json:"reserved"`
	PeerSet  string       `json:"peer_set"`
}

// Config is everything the service needs from the daemon around it.
type Config struct {
	Machine   string
	OwnerUser string // the local account sessions run as (--owner-user); a spec's owner must equal it
	Peers     []Peer // every OTHER machine, one shared list (PeerSet is the hash of ALL names incl. this one)
	PeerSet   string
	StateDir  string // <state>/remote

	Profiles  func(name string) (*profile.Profile, error)
	LookupUID func(user string) (int, error)
	// Starter is the local start with the stamped caller: start.Daemon.Start for the owner's uid.
	Starter func(uid int, spec []byte, startedBy string) (*start.StartReply, error)
	// Live lists this machine's live sessions of a label (the local record's view). An error means "could not
	// tell": it is never read as "nothing live" (the answer to a peer is then an error and the start refuses).
	Live func(label string) ([]View, error)
	// Recorded is the reply of a start this caller already made with this request_id (nil: none), read from the
	// session records: it recovers a reply the idempotency store lost (a crash between the launch and its write).
	Recorded func(startedBy, requestID, label string) (rep *start.StartReply, sameLabel bool)
	// RemoteEnabled: false = this daemon only runs the label check for starts (no POST /v1/sessions).
	RemoteEnabled bool

	AskPeer func(ctx context.Context, p Peer, label string) (Answer, error) // default: HTTP GET /v1/resolve?v=2

	PerUserMinute    int           // default 3
	PerMachineMinute int           // default 10
	AskTimeout       time.Duration // one total deadline for all peers, default 3 s
	ReserveTTL       time.Duration // a reservation not renewed for this long is dead, default 90 s
	IdemRetention    time.Duration // default 24 h
	Now              func() time.Time
	Rand             func(n int) int
	HTTP             *http.Client
	Logf             func(string, ...any) // the daemon's log (a failed start's real error goes here, never to the caller)
}

// Service is the remote start.
type Service struct {
	c Config

	// in-flight starts of THIS process, by label: authoritative for Answer whatever the clock says (the disk copy
	// and its TTL are only for crash recovery), and by idempotency key
	flight  map[string]string // label -> token
	keys    map[string]string // idempotency key -> token
	mu      sync.Mutex
	userWin map[string][]time.Time
	machWin map[string][]time.Time
}

// New builds a service and reconciles what a previous run left on disk.
func New(c Config) (*Service, error) {
	if c.PerUserMinute == 0 {
		c.PerUserMinute = 3
	}
	if c.PerMachineMinute == 0 {
		c.PerMachineMinute = 10
	}
	if c.AskTimeout == 0 {
		c.AskTimeout = 3 * time.Second
	}
	if c.ReserveTTL == 0 {
		c.ReserveTTL = 90 * time.Second
	}
	if c.IdemRetention == 0 {
		c.IdemRetention = 24 * time.Hour
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Rand == nil {
		c.Rand = rand.Intn
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	s := &Service{c: c, flight: map[string]string{}, keys: map[string]string{}, userWin: map[string][]time.Time{}, machWin: map[string][]time.Time{}}
	if c.AskPeer == nil {
		s.c.AskPeer = s.askHTTP
	}
	for _, d := range []string{"idem", "res"} {
		if err := os.MkdirAll(filepath.Join(c.StateDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	s.reconcile()
	return s, nil
}

// Enabled reports whether POST /v1/sessions is served (the label check always is).
func (s *Service) Enabled() bool { return s.c.RemoteEnabled }

// PeerSet is the hash of the set of machines this daemon is configured with (every daemon must agree).
func (s *Service) PeerSet() string { return s.c.PeerSet }

// PeerSetHash is the identity of the shared list of machines: every daemon must be configured with the same set.
func PeerSetHash(machines []string) string {
	l := append([]string(nil), machines...)
	sort.Strings(l)
	h := sha256.Sum256([]byte(strings.Join(l, "\n")))
	return hex.EncodeToString(h[:8])
}

// Status maps a refusal code to its HTTP status (docs/m2c-remote-start.md §1).
func Status(code string) int {
	switch code {
	case start.Forbidden, start.ProfileNotApproved, start.GrantNotAllowed, start.ResumeNotYours:
		return 403
	case start.AlreadyRunning, start.StartInProgress, start.SessionBusy:
		return 409
	case start.RateCapped:
		return 429
	case start.MachineFull, start.PeerUnreachable, start.PeerSetMismatch:
		return 503
	case start.UnsupportedMedia:
		return 415
	case start.BillingUnavailable, start.ImageNotPinned, start.WrongMachine, start.RequestIDReused, start.NotBuilt:
		return 422
	case Internal:
		return 500
	}
	return 400 // bad_spec and anything unnamed
}

func refusal(code, f string, a ...any) *start.Refusal {
	return &start.Refusal{Code: code, Msg: fmt.Sprintf(f, a...)}
}

// retry sets a jittered Retry-After (1-5 s) so two refusing machines do not retry in step.
func (s *Service) retry(r *start.Refusal) *start.Refusal {
	r.RetryAfter = 1 + s.c.Rand(5)
	return r
}

var (
	harnessValRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,99}$`)
	flagNameRe   = regexp.MustCompile(`^--[a-z][a-z0-9-]{0,40}$`)
)

// Start handles one remote start. The caller is already known to be the owner (the handler's job).
func (s *Service) Start(ctx context.Context, caller Caller, body []byte) (*start.StartReply, *start.Refusal) {
	spec, err := start.ParseSpec(body)
	if err != nil {
		return nil, asRefusal(err)
	}
	if spec.Machine != s.c.Machine {
		return nil, refusal(start.WrongMachine, "spec is for another machine")
	}
	if spec.GitDir != "" {
		return nil, refusal(start.BadSpec, "a remote start carries no gitdir")
	}
	if s.c.OwnerUser == "" || spec.Owner != s.c.OwnerUser {
		return nil, refusal(start.BadSpec, "owner is not this machine's owner user")
	}
	uid, uerr := s.c.LookupUID(s.c.OwnerUser)
	if uerr != nil {
		return nil, refusal(start.BadSpec, "this machine's owner user cannot be resolved")
	}
	p, perr := s.c.Profiles(spec.Profile)
	if perr != nil {
		return nil, refusal(start.ProfileNotApproved, "this profile is not approved in this build")
	}
	if m, _ := p.Allows(s.c.Machine, spec.Billing); !m {
		return nil, refusal(start.ProfileNotApproved, "this profile is not approved for this machine")
	}
	if r := s.policy(spec, p); r != nil {
		return nil, r
	}

	// Idempotency first: a retry of an answered request is answered again, even when this caller is capped now.
	hash := specHash(body)
	key := idemKey(caller.StartedBy, spec.RequestID)
	token := newToken()
	switch st, rep, r := s.idemBegin(key, hash, token); {
	case r != nil:
		return nil, r
	case st == "done":
		return rep, nil
	}
	// a reply the store lost (a crash between the launch and its write) is recovered from the session record
	if s.c.Recorded != nil {
		if rep, same := s.c.Recorded(caller.StartedBy, spec.RequestID, spec.Label); rep != nil {
			if !same { // the same caller reusing a request_id for ANOTHER session: not the old reply
				s.idemEnd(key, hash, token, nil, refusal(start.RequestIDReused, "request_id was already used for another session"))
				return nil, refusal(start.RequestIDReused, "request_id was already used for another session")
			}
			s.idemEnd(key, hash, token, rep, nil)
			return rep, nil
		}
	}
	rep, ref := s.run(ctx, caller, spec, uid, body)
	s.idemEnd(key, hash, token, rep, ref)
	return rep, ref
}

func (s *Service) run(ctx context.Context, caller Caller, spec *start.LaunchSpec, uid int, body []byte) (*start.StartReply, *start.Refusal) {
	if r := s.rate(caller); r != nil {
		return nil, r
	}
	rep, ref := s.Guard(ctx, spec.Label, caller.StartedBy, func() (*start.StartReply, error) {
		return s.c.Starter(uid, body, caller.StartedBy)
	})
	if ref != nil {
		switch ref.Code { // an attempt that never reached the launch does not use up the caller's minute
		case start.StartInProgress, start.PeerUnreachable, start.AlreadyRunning, start.PeerSetMismatch:
			s.unrate(caller)
		}
	}
	return rep, ref
}

func (s *Service) unrate(c Caller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range []map[string][]time.Time{s.userWin} {
		if w := m[c.StartedBy]; len(w) > 0 {
			m[c.StartedBy] = w[:len(w)-1]
		}
	}
	if w := s.machWin[c.Machine]; len(w) > 0 {
		s.machWin[c.Machine] = w[:len(w)-1]
	}
}

func newToken() string {
	var b [12]byte
	crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// policy is what a REMOTE caller may ask for beyond the local start's own checks (C2).
func (s *Service) policy(spec *start.LaunchSpec, p *profile.Profile) *start.Refusal {
	if len(spec.Grants) > 16 {
		return refusal(start.BadSpec, "at most 16 grants")
	}
	for _, g := range spec.Grants {
		if !p.GrantAllowed(g) {
			return refusal(start.GrantNotAllowed, "a requested grant is not allowed for this profile on this machine")
		}
	}
	args := spec.HarnessArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		if !flagNameRe.MatchString(name) || !p.HarnessArgAllowed(name) {
			return refusal(start.BadSpec, "harness_args: a flag is not allowed for this profile")
		}
		if !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			val, hasVal = args[i], true
		}
		if hasVal && !harnessValRe.MatchString(val) {
			return refusal(start.BadSpec, "harness_args: a flag value is not acceptable")
		}
	}
	return nil
}

// Internal is the code of a start that failed for a reason that is not the caller's: the message says nothing of
// it (the daemon's log has it), because an error text can carry paths and request content.
const Internal = "internal"

func asRefusal(err error) *start.Refusal {
	if r, ok := err.(*start.Refusal); ok {
		return r
	}
	return refusal(Internal, "the start failed on this machine (see the daemon's log)")
}

// ---- rates ----

func (s *Service) rate(c Caller) *start.Refusal {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.c.Now()
	hit := func(m map[string][]time.Time, k string, max int) (bool, time.Duration) {
		w := m[k][:0]
		for _, t := range m[k] {
			if now.Sub(t) < time.Minute {
				w = append(w, t)
			}
		}
		if len(w) >= max {
			m[k] = w
			return true, time.Minute - now.Sub(w[0])
		}
		m[k] = append(w, now)
		return false, 0
	}
	if capped, wait := hit(s.userWin, c.StartedBy, s.c.PerUserMinute); capped {
		r := refusal(start.RateCapped, "more than %d remote starts a minute for this caller", s.c.PerUserMinute)
		r.RetryAfter = int(wait/time.Second) + 1
		return r
	}
	if capped, wait := hit(s.machWin, c.Machine, s.c.PerMachineMinute); capped {
		r := refusal(start.RateCapped, "more than %d remote starts a minute from this machine", s.c.PerMachineMinute)
		r.RetryAfter = int(wait/time.Second) + 1
		return r
	}
	return nil
}

// ---- idempotency ----

type idemRec struct {
	Hash  string            `json:"hash"`
	State string            `json:"state"` // inflight | done
	Reply *start.StartReply `json:"reply,omitempty"`
	At    int64             `json:"at"`
}

// specHash is sha256 of the request WITHOUT its brief, canonically encoded (sorted keys, no whitespace): a
// retry never starts anything or applies a new brief, and cc-tree rewrites the brief between a lost answer and
// its retry, so the brief is not part of "the same request". Everything else, client.lid included, is.
func specHash(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	delete(m, "brief")
	b, _ := json.Marshal(m)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func idemKey(startedBy, rid string) string {
	h := sha256.Sum256([]byte(startedBy + "\x00" + rid))
	return hex.EncodeToString(h[:16])
}

func (s *Service) idemPath(key string) string {
	return filepath.Join(s.c.StateDir, "idem", key+".json")
}

func (s *Service) readIdem(key string) *idemRec {
	b, err := os.ReadFile(s.idemPath(key))
	if err != nil {
		return nil
	}
	var r idemRec
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	if s.c.Now().Unix()-r.At > int64(s.c.IdemRetention/time.Second) {
		os.Remove(s.idemPath(key))
		return nil
	}
	if r.State == "inflight" && s.c.Now().Unix()-r.At > int64(s.c.ReserveTTL/time.Second) {
		os.Remove(s.idemPath(key)) // an attempt that died with a previous daemon
		return nil
	}
	return &r
}

func (s *Service) writeIdem(key string, r idemRec) {
	b, _ := json.Marshal(r)
	tmp := s.idemPath(key) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.idemPath(key))
	}
}

// idemBegin: "" (go on, marked in flight), "done" with the first reply, or a refusal (in flight: retry later;
// the same request_id with a different request: request_id_reused).
func (s *Service) idemBegin(key, hash, token string) (string, *start.StartReply, *start.Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, mine := s.keys[key]; mine { // an attempt of THIS process is still running: authoritative, whatever the clock says
		return "", nil, s.retry(refusal(start.StartInProgress, "this request is still being started: retry"))
	}
	if r := s.readIdem(key); r != nil {
		switch {
		case r.Hash != hash:
			return "", nil, refusal(start.RequestIDReused, "request_id was already used with a different request")
		case r.State == "done" && r.Reply != nil:
			return "done", r.Reply, nil
		default:
			return "", nil, s.retry(refusal(start.StartInProgress, "this request is still being started: retry"))
		}
	}
	s.keys[key] = token
	s.writeIdem(key, idemRec{Hash: hash, State: "inflight", At: s.c.Now().Unix()})
	return "", nil, nil
}

// idemEnd keeps the reply of a start that happened; a refused or failed attempt created nothing, so it leaves
// no trace and a retry is a fresh attempt.
func (s *Service) idemEnd(key, hash, token string, rep *start.StartReply, ref *start.Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys[key] == token {
		delete(s.keys, key)
	}
	if rep != nil && ref == nil {
		s.writeIdem(key, idemRec{Hash: hash, State: "done", Reply: rep, At: s.c.Now().Unix()})
		return
	}
	// a refused attempt removes only its OWN in-flight marker, never a stored reply of a start that happened
	if r := s.readIdem(key); r != nil && r.State != "done" {
		os.Remove(s.idemPath(key))
	}
}

// ---- one live generation per label, across machines ----

type resFile struct {
	Label     string `json:"label"`
	Machine   string `json:"machine"`
	Token     string `json:"token"`
	RequestID string `json:"request_id,omitempty"`
	StartedBy string `json:"started_by"`
	TS        int64  `json:"ts"`      // seconds
	Renewed   int64  `json:"renewed"` // milliseconds: a reservation lives for ReserveTTL after its last renewal
}

func (s *Service) resPath(label string) string {
	h := sha256.Sum256([]byte(label))
	return filepath.Join(s.c.StateDir, "res", hex.EncodeToString(h[:12])+".json")
}

// readResRaw reads a reservation file whatever its age.
func (s *Service) readResRaw(label string) *resFile {
	b, err := os.ReadFile(s.resPath(label))
	if err != nil {
		return nil
	}
	var r resFile
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func (s *Service) readRes(label string) *resFile {
	b, err := os.ReadFile(s.resPath(label))
	if err != nil {
		return nil
	}
	var r resFile
	if json.Unmarshal(b, &r) != nil || s.c.Now().UnixMilli()-r.Renewed > s.c.ReserveTTL.Milliseconds() {
		return nil // dead: not renewed within its time
	}
	return &r
}

// writeRes writes a reservation through a temp name of its own (process and token), O_EXCL, then renames; an error means
// the reservation does NOT exist and the caller must refuse.
func (s *Service) writeRes(r resFile) error {
	b, _ := json.Marshal(r)
	tmp := fmt.Sprintf("%s.%d.%s.tmp", s.resPath(r.Label), os.Getpid(), r.Token)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.resPath(r.Label)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// fileLock serialises the reservation check/create/renew/release across PROCESSES (hostd and a direct-mode CLI share the
// directory): without it two of them could both see "free" and both reserve.
func (s *Service) fileLock() func() {
	f, err := os.OpenFile(filepath.Join(s.c.StateDir, "res.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}

// reconcile drops what a previous daemon run left that is past its time. A reservation still inside its time
// stays and expires by itself; a launch that was in flight when the daemon died left, at worst, a record that
// the local live check sees.
func (s *Service) reconcile() {
	for _, d := range []string{"res", "idem"} {
		ents, _ := os.ReadDir(filepath.Join(s.c.StateDir, d))
		for _, e := range ents {
			p := filepath.Join(s.c.StateDir, d, e.Name())
			if strings.HasSuffix(e.Name(), ".tmp") {
				os.Remove(p)
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var m struct {
				At      int64  `json:"at"`
				Renewed int64  `json:"renewed"`
				State   string `json:"state"`
			}
			json.Unmarshal(b, &m)
			age := s.c.Now().Unix() - m.At
			if (d == "res" && s.c.Now().UnixMilli()-m.Renewed > s.c.ReserveTTL.Milliseconds()) ||
				(d == "idem" && m.State == "inflight" && age > int64(s.c.ReserveTTL/time.Second)) ||
				(d == "idem" && age > int64(s.c.IdemRetention/time.Second)) {
				os.Remove(p)
			}
		}
	}
}

// Answer is this machine's view of a label, for GET /v1/resolve?v=2 (a peer asking, or this daemon's own check). An
// error means "could not tell": the caller answers an error, never an empty list (that would read as "free").
func (s *Service) Answer(label string) (Answer, error) {
	live, err := s.c.Live(label)
	if err != nil {
		if s.c.Logf != nil {
			s.c.Logf("label check for %s: cannot read the session records: %v", label, err)
		}
		return Answer{}, err
	}
	a := Answer{Live: live, PeerSet: s.c.PeerSet}
	if a.Live == nil {
		a.Live = []View{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.flight[label]; ok { // this process is starting it: true whatever the timestamps say
		a.Reserved = &Reservation{Machine: s.c.Machine, TS: s.c.Now().Unix()}
	} else if r := s.readRes(label); r != nil { // another life of this daemon left it (crash recovery, by TTL)
		a.Reserved = &Reservation{Machine: r.Machine, TS: r.TS}
	}
	return a, nil
}

// Guard runs launch only if no machine has the label live or reserved (whatever the machines' names: no
// tie-break is trusted). It reserves the label here (in memory and on disk, renewed until the launch is over), asks
// every listed peer in parallel under one deadline, and drops the reservation on any doubt. Both sides refusing
// and retrying is safe; neither can start two generations. The daemon's lock is NOT held while peers are asked.
// Each call holds its own random token: a reservation is never "ours" because a request_id matches.
func (s *Service) Guard(ctx context.Context, label, startedBy string, launch func() (*start.StartReply, error)) (*start.StartReply, *start.Refusal) {
	// 1. here: live or reserved by someone else? then reserve (briefly under the service lock)
	live, err := s.c.Live(label)
	if err != nil {
		if s.c.Logf != nil {
			s.c.Logf("start of %s refused: cannot read the session records: %v", label, err)
		}
		return nil, refusal(Internal, "the start cannot be checked on this machine (see the daemon's log)")
	}
	if len(live) > 0 {
		r := refusal(start.AlreadyRunning, "the label is already running")
		r.Machine, r.Sid, r.State = live[0].Machine, live[0].Sid, live[0].State
		return nil, r
	}
	token := newToken()
	s.mu.Lock()
	unlock := s.fileLock()
	if _, busy := s.flight[label]; busy || s.readRes(label) != nil {
		unlock()
		s.mu.Unlock()
		return nil, s.retry(refusal(start.StartInProgress, "the label is being started here"))
	}
	now := s.c.Now()
	if err := s.writeRes(resFile{Label: label, Machine: s.c.Machine, Token: token, StartedBy: startedBy, TS: now.Unix(), Renewed: now.UnixMilli()}); err != nil {
		unlock()
		s.mu.Unlock()
		if s.c.Logf != nil {
			s.c.Logf("start of %s refused: the reservation cannot be written: %v", label, err)
		}
		return nil, refusal(Internal, "the start cannot be reserved on this machine (see the daemon's log)")
	}
	s.flight[label] = token
	unlock()
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		unlock := s.fileLock()
		if s.flight[label] == token {
			delete(s.flight, label)
		}
		if r := s.readResRaw(label); r != nil && r.Token == token {
			os.Remove(s.resPath(label))
		}
		unlock()
		s.mu.Unlock()
	}
	defer release()

	// 2. every listed peer, in parallel, one total deadline
	if r := s.askAll(ctx, label); r != nil {
		return nil, r
	}

	// 3. launch, renewing the reservation until the outcome is known. The renewer rewrites by token and does not ask
	// whether the old stamp is still "live": a late tick (a sleeping laptop, a clock step) must not end the renewals.
	stop := make(chan struct{})
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		t := time.NewTicker(s.c.ReserveTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.mu.Lock()
				unlock := s.fileLock()
				if s.flight[label] == token {
					n := s.c.Now()
					s.writeRes(resFile{Label: label, Machine: s.c.Machine, Token: token, StartedBy: startedBy, TS: n.Unix(), Renewed: n.UnixMilli()})
				}
				unlock()
				s.mu.Unlock()
			}
		}
	}()
	rep, err := launch()
	close(stop)
	<-renewed
	if err != nil {
		if _, ok := err.(*start.Refusal); !ok && s.c.Logf != nil {
			s.c.Logf("start of %s failed: %v", label, err)
		}
		return nil, asRefusal(err)
	}
	return rep, nil
}

func (s *Service) askAll(ctx context.Context, label string) *start.Refusal {
	if len(s.c.Peers) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.c.AskTimeout)
	defer cancel()
	type res struct {
		p   Peer
		a   Answer
		err error
	}
	out := make(chan res, len(s.c.Peers))
	for _, p := range s.c.Peers {
		go func(p Peer) {
			a, err := s.c.AskPeer(ctx, p, label)
			out <- res{p, a, err}
		}(p)
	}
	var live, reserved *res
	for range s.c.Peers {
		r := <-out
		r2 := r
		switch {
		case r.err != nil:
			// one listed peer being down refuses every start on every machine (docs/m2c-remote-start.md §3): a guess either way breaks the rule
			return s.retry(refusal(start.PeerUnreachable, "peer %s could not be asked: %v", r.p.Name, r.err))
		case r.a.PeerSet != s.c.PeerSet:
			return refusal(start.PeerSetMismatch, "peer %s is configured with a different set of machines", r.p.Name)
		case len(r.a.Live) > 0 && live == nil:
			live = &r2
		case r.a.Reserved != nil && reserved == nil:
			reserved = &r2
		}
	}
	if live != nil {
		v := live.a.Live[0]
		r := refusal(start.AlreadyRunning, "%s is already running on %s", label, live.p.Name)
		r.Machine, r.Sid, r.State = live.p.Name, v.Sid, v.State
		return r
	}
	if reserved != nil {
		return s.retry(refusal(start.StartInProgress, "%s is being started on %s", label, reserved.p.Name))
	}
	return nil
}

// askHTTP is the default AskPeer: GET <peer>/v1/resolve?label=&v=2. No redirects, a bounded body.
func (s *Service) askHTTP(ctx context.Context, p Peer, label string) (Answer, error) {
	u := strings.TrimRight(p.URL, "/") + "/v1/resolve?v=2&label=" + url.QueryEscape(label)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return Answer{}, err
	}
	resp, err := s.c.HTTP.Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return Answer{}, fmt.Errorf("answered %d", resp.StatusCode)
	}
	var a Answer
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&a); err != nil {
		return Answer{}, fmt.Errorf("unreadable answer")
	}
	return a, nil
}

// Published is what hostd writes to <state>/remote/peers.json so that a start made WITHOUT hostd in the loop (a
// direct-mode `ccsbx start` runs the start in its own process) asks the same peers, reserves in the same place and
// refuses on the same doubts: one rule for every start path.
type Published struct {
	Machine      string `json:"machine"`
	Peers        []Peer `json:"peers"`
	PeerSet      string `json:"peer_set"`
	AskTimeoutMs int    `json:"ask_timeout_ms"`
}

// Publish writes the file (0644: a start by the same owner reads it).
func (s *Service) Publish() error {
	b, _ := json.Marshal(Published{Machine: s.c.Machine, Peers: s.c.Peers, PeerSet: s.c.PeerSet, AskTimeoutMs: int(s.c.AskTimeout / time.Millisecond)})
	p := filepath.Join(s.c.StateDir, "peers.json")
	if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// Unpublish removes a stale peers.json (this daemon runs without a peer list now).
func Unpublish(stateDir string) { os.Remove(filepath.Join(stateDir, "remote", "peers.json")) }

// LoadPublished reads it: (nil, nil) when hostd never published one (no cross-machine check configured), an
// error when the file exists and cannot be read (fail closed: the caller must refuse, not skip the check).
func LoadPublished(stateDir string) (*Published, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "remote", "peers.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Published
	if err := json.Unmarshal(b, &p); err != nil || p.Machine == "" {
		return nil, fmt.Errorf("peers.json is not readable")
	}
	// the peer-set hash is recomputed from the names, never taken from the file
	names := []string{p.Machine}
	for _, q := range p.Peers {
		names = append(names, q.Name)
	}
	p.PeerSet = PeerSetHash(names)
	return &p, nil
}
