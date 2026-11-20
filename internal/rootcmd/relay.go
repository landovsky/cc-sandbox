// relay.go: `ccsbx-root relay-mint` and the relay's revoke (spec: docs/relay-mint-spec.md, decision D2).
// It exists so hostd can send a signed message across machines without ever holding the session bus
// hub's admin secret: hostd keeps the generation's private key, root registers the public half with
// the hub as a `relay` principal for ONE session generation. The hub's admin secret is read here and
// nowhere else, only from a root-owned 0600 file, only after every check on the record has passed, and
// it is never written to any output. The hub is on this machine (loopback), so this is hp only.
package rootcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

const (
	relayRateWindow = 10 * time.Second
	hubTimeout      = 10 * time.Second
	exitHub         = 3 // the hub or the secret
)

var (
	pubRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	genRe     = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
	mrMachine = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	mrNode    = regexp.MustCompile(`^[A-Za-z0-9._#:/-]{1,128}$`)
)

func (e *env) now() time.Time {
	if e.d.Now != nil {
		return e.d.Now()
	}
	return time.Now()
}

func (e *env) audit(verb, sid string, gen int, pub, result string) {
	var w io.Writer = os.Stderr
	if e.d.Audit != nil {
		w = e.d.Audit
	}
	h := "-"
	if pub != "" {
		sum := sha256.Sum256([]byte(pub))
		h = hex.EncodeToString(sum[:])[:8]
	}
	fmt.Fprintf(w, "ccsbx-root audit: %s %s sid=%s gen=%d pub=%s result=%s\n", e.now().UTC().Format(time.RFC3339), verb, sid, gen, h, result)
}

func (e *env) relayMintCmd() *cli.Command {
	return &cli.Command{
		Name:    "relay-mint",
		Summary: "register the public half of a generation's key with the hub as a relay principal; prints {\"token\":...}",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root relay-mint -- s0k1dry66qg 3 <pub, 43 chars url-safe base64>
  sudo -n /usr/local/sbin/ccsbx-root relay-mint --after-401 -- s0k1dry66qg 3 <pub>
  For the service user (hostd) only. The record must be this machine's current, running generation. What
  the relay may reach comes from the record (the session's manager machine/node), never from an argument.
  Needs /etc/ccsbx/hub-admin.secret (root, 0600) and a loopback hub_url. One mint per generation per 10 s;
  --after-401 allows one more inside that window after the hub refused the previous token.
  Exit 2: refused. Exit 3: the hub or the secret (the status code only on stderr).`,
		Setup: func(f *cli.Flags) {
			f.Bool("after-401", false, "allow ONE mint inside the 10 s window because the hub refused the previous relay token")
		},
		Run: func(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
			if len(args) != 3 {
				fmt.Fprintln(stderr, "ccsbx-root relay-mint: refused: usage: relay-mint [--after-401] -- <sid> <gen> <pub>   (nothing else is accepted)")
				return cli.ExitRefused
			}
			after401 := f.Lookup("after-401").Value.String() == "true"
			if err := e.relayMint(args[0], args[1], args[2], after401, stdout); err != nil {
				return fail(stderr, "relay-mint", err)
			}
			return cli.ExitOK
		},
	}
}

func (e *env) relayMint(sid, genS, pub string, after401 bool, stdout io.Writer) error {
	w, err := e.prelude("relay-mint")
	if err != nil {
		return err
	}
	if w.uid != w.service {
		return refuse("relay-mint is for the service user only")
	}
	if !sidMintRe.MatchString(sid) {
		return refuse("%q is not a session id", sid)
	}
	if !genRe.MatchString(genS) {
		return refuse("%q is not a generation", genS)
	}
	if !pubRe.MatchString(pub) {
		return refuse("pub must be 43 characters of url-safe base64 (a 32-byte key)")
	}
	gen, _ := strconv.Atoi(genS)
	rec, err := e.session(w, sid) // trusted record, the caller's right to it, a docker session, this machine
	if err != nil {
		return err
	}
	if rec.Gen != gen || rec.Ended != nil {
		return refuse("stale_generation: session %s is at generation %d (ended: %v), not %d", sid, rec.Gen, rec.Ended != nil, gen)
	}
	gdir := filepath.Join(e.cfg.RunDir, "s", sid, "g"+genS)
	if st, err := os.Lstat(filepath.Join(gdir, "PID")); err != nil || !st.Mode().IsRegular() {
		return refuse("generation %d of %s is not running (no PID)", gen, sid)
	}
	if _, err := os.Lstat(filepath.Join(gdir, "CLOSED")); err == nil {
		return refuse("generation %d of %s is closed", gen, sid)
	}
	if err := e.rateLimit(filepath.Join(gdir, "relay-mint.stamp"), after401); err != nil {
		return err
	}
	// everything checked: only now is the secret read
	secret, err := e.hubSecret()
	if err != nil {
		e.audit("relay-mint", sid, gen, pub, "no secret")
		return &codeError{exitHub, "the hub admin secret is not usable: " + err.Error()}
	}
	tok, err := e.registerRelay(context.Background(), secret, rec.Spec.MsgScope, sid, gen, pub)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]string{"token": tok})
}

// registerRelay asks the hub for a relay principal for one generation: its public key, and what it may
// reach from the record's scope. The secret is the caller's, already read from its trusted file. The
// token comes back to the caller only (the verb prints it; pod-load puts it in the sidecar's payload).
func (e *env) registerRelay(ctx context.Context, secret string, scope json.RawMessage, sid string, gen int, pub string) (string, error) {
	body, _ := json.Marshal(map[string]any{"role": "relay", "sid": sid, "gen": strconv.Itoa(gen), "pub": pub, "may_reach": mayReach(scope)})
	code, resp, err := e.hubCall(ctx, "POST", "/v1/principals", secret, body)
	if err != nil {
		e.audit("relay-mint", sid, gen, pub, "unreachable")
		return "", &codeError{exitHub, "the hub is not reachable"}
	}
	var out struct{ Token string }
	if code != http.StatusCreated || json.Unmarshal(resp, &out) != nil || out.Token == "" {
		e.audit("relay-mint", sid, gen, pub, "hub "+strconv.Itoa(code))
		return "", &codeError{exitHub, "the hub answered " + strconv.Itoa(code)}
	}
	e.audit("relay-mint", sid, gen, pub, "ok")
	return out.Token, nil
}

var sidMintRe = regexp.MustCompile(`^s[0-9a-z]{4,32}$`)

// mayReach is derived from the record and never supplied: the session's manager as <machine>/<node>.
// A record that names no manager machine (or a malformed one) gives an empty list: the relay then
// reaches fenced sessions only. Peers are not in it.
func mayReach(scope json.RawMessage) []string {
	var s struct {
		Manager struct{ Node, Machine string }
	}
	if json.Unmarshal(scope, &s) != nil || !mrMachine.MatchString(s.Manager.Machine) || !mrNode.MatchString(s.Manager.Node) {
		return []string{}
	}
	return []string{s.Manager.Machine + "/" + s.Manager.Node}
}

// rateLimit: one mint per generation per 10 s; --after-401 allows one more inside the window. The
// stamp is written before the hub is asked, so a failing hub is not hammered either.
func (e *env) rateLimit(path string, after401 bool) error {
	now := e.now()
	var last int64
	used := false
	if b, err := os.ReadFile(path); err == nil {
		f := strings.Fields(string(b))
		if len(f) == 2 {
			last, _ = strconv.ParseInt(f[0], 10, 64)
			used = f[1] == "1"
		}
	}
	inWindow := last != 0 && now.Sub(time.Unix(last, 0)) < relayRateWindow
	switch {
	case inWindow && after401 && !used:
		used = true
	case inWindow:
		return refuse("rate: one relay mint per generation per %s", relayRateWindow)
	default:
		last, used = now.Unix(), false
	}
	u := "0"
	if used {
		u = "1"
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d %s\n", last, u)), 0o600)
}

// hubSecret reads the hub admin secret: a regular, root-owned file nobody else can read or write, in a
// directory likewise, never a link. It returns the secret without the trailing newline.
func (e *env) hubSecret() (string, error) {
	p := e.cfg.HubSecret
	st, err := os.Lstat(p)
	if err != nil {
		return "", fmt.Errorf("missing")
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("a symlink")
	}
	if err := trustedFile(p, e.cfg.TrustedUID, true); err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("accessible to group or others (%v)", st.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" || len(s) > 512 || strings.ContainsAny(s, "\r\n\x00") {
		return "", fmt.Errorf("empty or malformed")
	}
	return s, nil
}

func (e *env) hubClient() *http.Client {
	if e.d.Hub != nil {
		return e.d.Hub
	}
	return &http.Client{Timeout: hubTimeout, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// hubCall makes one request with the admin secret as bearer and returns the status and a bounded body.
func (e *env) hubCall(ctx context.Context, method, path, secret string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(e.cfg.HubURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.hubClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

// revokeRelay deletes the relay principal of a generation that has ended (run's cleanup, stop, gc).
// Best effort: the hub's own 6 h relay lifetime is the backstop, and no failure here stops the caller.
func (e *env) revokeRelay(sid string, gen int) {
	secret, err := e.hubSecret()
	if err != nil {
		e.audit("relay-revoke", sid, gen, "", "skipped: "+err.Error())
		return
	}
	e.revokeWith(secret, sid, gen)
}

// revokeWith is revokeRelay for a caller that already holds the secret in memory (pod-load, after the
// drop, when its own load fails).
func (e *env) revokeWith(secret, sid string, gen int) {
	id := url.PathEscape("relay:" + sid + "/" + strconv.Itoa(gen)) // one path segment
	code, _, err := e.hubCall(context.Background(), "DELETE", "/v1/principals/"+id, secret, nil)
	switch {
	case err != nil:
		e.audit("relay-revoke", sid, gen, "", "unreachable")
	case code/100 == 2 || code == http.StatusNotFound:
		e.audit("relay-revoke", sid, gen, "", "ok")
	default:
		e.audit("relay-revoke", sid, gen, "", "hub "+strconv.Itoa(code))
	}
}
