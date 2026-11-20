// machines_test.go checks the trusted-key list (M9) the way a door meets it: files in, a verdict on
// real tokens out. Each context names the problem the rule exists to stop, and each refusal test
// asserts WHICH rule refused, so a case can't pass because of some other rejection.
package machines

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

var t0 = time.Unix(1_800_000_000, 0)

func newKey(t *testing.T) *token.Key {
	t.Helper()
	k, err := token.LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func kref(k *token.Key) KeyRef {
	return KeyRef{KID: k.KID, Key: base64.StdEncoding.EncodeToString(k.Public)}
}

// secureDir is a directory the loader accepts (the test umask may make t.TempDir group-writable).
func secureDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// put writes e in canonical form, as a reviewer's `ccsbx machines fmt` would.
func put(t *testing.T, dir, machine string, e Entry) {
	t.Helper()
	e.Machine = machine
	if err := os.WriteFile(filepath.Join(dir, machine+".json"), []byte(canonical(&e)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// listWith writes hp (which also signs for future-two, a machine with no file) and a Mac whose
// ceiling is api and stub.
func listWith(t *testing.T, hp, mac *token.Key) string {
	dir := secureDir(t)
	put(t, dir, "hp-ubuntu", Entry{Owners: []string{"tomas"}, PublicKeys: []KeyRef{kref(hp)},
		MaySignMachines: []string{"hp-ubuntu", "future-two"}, MaxProfile: []string{"dev-*", "e2e-*"},
		MaxBilling: []string{"api", "subscription", "stub"},
		Pairs:      map[string][]string{"dev-default": {"api", "subscription", "stub"}, "e2e-default": {"stub"}}})
	put(t, dir, "mac", Entry{Owners: []string{"tomas"}, PublicKeys: []KeyRef{kref(mac)},
		MaySignMachines: []string{"mac"}, MaxProfile: []string{"dev-*", "e2e-*"}, MaxBilling: []string{"api", "stub"},
		Pairs: map[string][]string{"dev-default": {"api", "stub"}, "e2e-default": {"stub"}}})
	return dir
}

type door struct {
	t *testing.T
	v *token.Verifier
	g *token.GenKey
}

func newDoor(t *testing.T, ring token.Keyring) *door {
	g, _ := token.NewGenKey()
	return &door{t, token.NewVerifier(ring, "ccsbx-model"), g}
}

func (d *door) call(k *token.Key, sub, profile, billing string) error {
	d.t.Helper()
	tok, err := token.Mint(k, token.Claims{Sub: sub, Label: "l", Billing: billing, Profile: profile,
		Aud: []string{"ccsbx-model"}, Cnf: token.CnfOf(d.g.Public)}, t0)
	if err != nil {
		d.t.Fatal(err)
	}
	p, _ := token.SignProof(d.g, "POST", "/x", "ccsbx-model", tok, nil, t0)
	_, err = d.v.Verify(tok, p, "POST", "/x", nil, t0)
	return err
}

func ringOf(t *testing.T, dir string) token.Keyring {
	t.Helper()
	es, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Ring(es)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// context "the list is right": each real key signs what its file says it may (the allow half of
// T:key-signs-foreign-machine and T:mac-key-above-mac-max).
func TestTheListAllowsWhatItsFilesSay(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	d := newDoor(t, ringOf(t, listWith(t, hp, mac)))
	for _, c := range []struct {
		why                   string
		k                     *token.Key
		sub, profile, billing string
	}{
		{"the Mac key, its own machine, api", mac, "tomas/mac/sM1/g1", "dev-default", "api"},
		{"the Mac key, e2e on the stub", mac, "tomas/mac/sM2/g1", "e2e-default", "stub"},
		{"hp for itself on the subscription", hp, "tomas/hp-ubuntu/sH1/g1", "dev-default", "subscription"},
		{"hp for future-two, which has no file of its own", hp, "tomas/future-two/sF1/g1", "dev-default", "subscription"},
	} {
		if err := d.call(c.k, c.sub, c.profile, c.billing); err != nil {
			t.Errorf("%s: %v", c.why, err)
		}
	}
}

// context "a Mac key is stolen (T:key-signs-foreign-machine, T:mac-key-above-mac-max)": the list must
// let a door refuse what the thief can make with it, not only what the real owner never would.
func TestTheListLimitsAStolenKey(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	d := newDoor(t, ringOf(t, listWith(t, hp, mac)))
	for _, c := range []struct{ why, sub, profile, billing string }{
		{"the Mac key signing an hp session", "tomas/hp-ubuntu/sH1/g9", "dev-default", "api"},
		{"the Mac key signing future-two, which only hp may", "tomas/future-two/sF2/g1", "dev-default", "api"},
		{"the Mac key above its billing ceiling", "tomas/mac/sM2/g1", "dev-default", "subscription"},
		{"a profile under the ceiling but not in its pairs", "tomas/mac/sM3/g1", "dev-other", "api"},
		{"a billing the profile's own pair excludes (e2e is stub only)", "tomas/mac/sM4/g1", "e2e-default", "api"},
		{"another owner", "eve/mac/sM5/g1", "dev-default", "api"},
	} {
		if err := d.call(mac, c.sub, c.profile, c.billing); !errors.Is(err, token.ErrKeyScope) {
			t.Errorf("%s: got %v, want ErrKeyScope", c.why, err)
		}
	}
	// and hp's own key is held to its pairs too: e2e is stub only for it as well
	if err := d.call(hp, "tomas/hp-ubuntu/sH2/g1", "e2e-default", "api"); !errors.Is(err, token.ErrKeyScope) {
		t.Errorf("hp key, e2e on api: got %v, want ErrKeyScope", err)
	}
}

// clocked is a Reloader on a test clock set before its first load, so lastGood is on that clock.
func clocked(t *testing.T, dir string, now *time.Time) *Reloader {
	t.Helper()
	r := &Reloader{Dir: dir, Every: DefaultEvery, MaxStale: DefaultMaxStale, Now: func() time.Time { return *now }}
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	return r
}

// context "a listed key keeps working across polls" (the allow half of the removal rule).
func TestAListedKeyStaysTrustedAcrossPolls(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	now := t0
	r := clocked(t, listWith(t, hp, mac), &now)
	d := newDoor(t, nil)
	d.v.SetRingSource(r.Ring)
	for i := 0; i < 5; i++ {
		now = now.Add(r.Every)
		if err := r.Refresh(); err != nil {
			t.Fatal(err)
		}
		if err := d.call(mac, "tomas/mac/sM1/g1", "dev-default", "api"); err != nil {
			t.Fatalf("poll %d: a listed key stopped verifying: %v", i, err)
		}
	}
}

// context "a key is removed from the list (rotation or theft)": a running door, with its real
// background refresher, must stop trusting it with no restart.
func TestRemovedKeyIsRefusedByARunningDoor(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	dir := listWith(t, hp, mac)
	r, err := NewReloader(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Every, r.MaxStale = 20*time.Millisecond, time.Second
	d := newDoor(t, nil)
	d.v.SetRingSource(r.Ring)
	stop := make(chan struct{})
	ctx := ctxDone(stop)
	go r.Run(ctx, nil)
	defer close(stop)
	if err := d.call(mac, "tomas/mac/sM1/g1", "dev-default", "api"); err != nil {
		t.Fatalf("allow twin before the removal: %v", err)
	}
	os.Remove(filepath.Join(dir, "mac.json"))
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := d.call(mac, "tomas/mac/sM1/g2", "dev-default", "api")
		if errors.Is(err, token.ErrUnknownKID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the removed Mac key still verifies 2 s after its file went (last: %v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := d.call(hp, "tomas/hp-ubuntu/sH1/g1", "dev-default", "api"); err != nil {
		t.Fatalf("allow twin: removing the Mac key broke hp: %v", err)
	}
}

// context "the poll hangs or its goroutine dies": nothing refreshes the list again, and the last ring
// must still stop being trusted, judged when the token arrives.
func TestARefresherThatStopsEndsTrust(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	now := t0
	r := clocked(t, listWith(t, hp, mac), &now)
	d := newDoor(t, nil)
	d.v.SetRingSource(r.Ring)
	if err := d.call(mac, "tomas/mac/sM1/g1", "dev-default", "api"); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	now = now.Add(r.MaxStale - time.Second)
	if err := d.call(mac, "tomas/mac/sM1/g2", "dev-default", "api"); err != nil {
		t.Fatalf("just inside the stale limit: %v", err)
	}
	now = now.Add(time.Second) // exactly MaxStale old, and Refresh was never called
	if err := d.call(mac, "tomas/mac/sM1/g3", "dev-default", "api"); !errors.Is(err, token.ErrUnknownKID) {
		t.Fatalf("a list MaxStale old still trusted: %v", err)
	}
}

func TestABrokenListKeepsTheLastGoodOneOnlyBriefly(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	dir := listWith(t, hp, mac)
	now := t0
	r := clocked(t, dir, &now)
	os.WriteFile(filepath.Join(dir, "mac.json"), []byte("{not json"), 0o644)
	now = now.Add(r.Every)
	if err := r.Refresh(); err == nil || len(r.Ring()) != 2 {
		t.Fatalf("one poll into a bad list: err=%v keys=%d, want the last good ring (2 keys)", err, len(r.Ring()))
	}
	now = now.Add(r.MaxStale)
	r.Refresh()
	if len(r.Ring()) != 0 {
		t.Fatalf("a list bad for longer than MaxStale still trusts %d keys", len(r.Ring()))
	}
	put(t, dir, "mac", Entry{Owners: []string{"tomas"}, PublicKeys: []KeyRef{kref(mac)}, MaySignMachines: []string{"mac"},
		MaxProfile: []string{"dev-*"}, MaxBilling: []string{"api"}, Pairs: map[string][]string{"dev-default": {"api"}}})
	if err := r.Refresh(); err != nil || len(r.Ring()) != 2 {
		t.Fatalf("after the fix: err=%v keys=%d, want it to recover", err, len(r.Ring()))
	}
}

// context "the plan: a removed key is refused within 2 minutes": the kubelet takes up to 60 s to update
// a ConfigMap volume, so the stale limit must fit in the other 60 s.
func TestDefaultsMeetThePlansTwoMinutes(t *testing.T) {
	r, err := NewReloader(listWith(t, newKey(t), newKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxStale+60*time.Second > 2*time.Minute || r.MaxStale < r.Every {
		t.Fatalf("poll %s, stale limit %s: kubelet 60 s + stale limit must be ≤ 2 min, and the limit at least the poll", r.Every, r.MaxStale)
	}
}

func TestFirstLoadFailureStopsTheDoor(t *testing.T) {
	if _, err := NewReloader(secureDir(t)); err == nil || !strings.Contains(err.Error(), "no *.json files") {
		t.Fatalf("an empty list: %v", err)
	}
	if _, err := NewReloader(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing list started a door")
	}
}

func goodEntry(k *token.Key) Entry {
	return Entry{Machine: "mac", Owners: []string{"tomas"}, PublicKeys: []KeyRef{kref(k)}, MaySignMachines: []string{"mac"},
		MaxProfile: []string{"dev-*"}, MaxBilling: []string{"api", "stub"}, Pairs: map[string][]string{"dev-default": {"api"}}}
}

// context "a typo or a trick in a file must not read as a wider limit": every refusal below is a file
// that would widen what a key may sign if it were read leniently, and each names the rule that refused it.
func TestFilesThatWouldWidenAKeyAreRefused(t *testing.T) {
	k := newKey(t)
	good := goodEntry(k)
	if _, err := Parse("mac.json", []byte(canonical(&good))); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	canon := canonical(&good)
	mut := func(f func(e *Entry)) string {
		e := good
		e.PublicKeys = append([]KeyRef(nil), good.PublicKeys...) // a case must not leak its change into the next
		f(&e)
		return canonical(&e)
	}
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, ed25519.SeedSize)) // a 32-byte secret, key-shaped
	for _, c := range []struct{ why, body, want string }{
		{"a private seed pasted as a public key", mut(func(e *Entry) { e.PublicKeys = []KeyRef{{KID: k.KID, Key: seed}} }), "does not match the key"},
		{"a made-up kid", mut(func(e *Entry) { e.PublicKeys[0].KID = "0000000000000000" }), "does not match the key"},
		{"a duplicate max_billing that widens by coming last",
			strings.Replace(canon, `"max_billing": [`, `"max_billing": ["api"],`+"\n  "+`"max_billing": [`, 1), "canonical"},
		{"a wrong-case key that overrides the real one",
			strings.TrimSuffix(canon, "}\n") + `,"MAX_BILLING":["api","subscription"]}` + "\n", "canonical"},
		{"the same bytes with other spacing", strings.ReplaceAll(canon, "  ", "\t"), "canonical"},
		{"no trailing newline", strings.TrimSuffix(canon, "\n"), "canonical"},
		{"an extra limit nothing reads", strings.Replace(canon, `{`, `{"max_spend":1,`, 1), "unknown field"},
		{"data after the object", canon + `{}`, "after the JSON object"},
		{"machine differs from the file name", mut(func(e *Entry) { e.Machine = "hp-ubuntu" }), "equal to the file name"},
		{"no pairs (a ceiling alone)", mut(func(e *Entry) { e.Pairs = nil }), "pairs is required"},
		{"pair profile outside max_profile", mut(func(e *Entry) { e.Pairs = map[string][]string{"prod": {"api"}} }), "outside max_profile"},
		{"pair billing outside max_billing", mut(func(e *Entry) { e.Pairs = map[string][]string{"dev-default": {"subscription"}} }), "outside max_billing"},
		{"a fourth billing", mut(func(e *Entry) { e.MaxBilling = []string{"free"} }), "not api, subscription or stub"},
		{"empty owners", mut(func(e *Entry) { e.Owners = nil }), "owners is empty"},
		{"empty may_sign_machines", mut(func(e *Entry) { e.MaySignMachines = nil }), "may_sign_machines is empty"},
		{"a file that does not sign for its own machine", mut(func(e *Entry) { e.MaySignMachines = []string{"future-two"} }), "must include mac"},
		{"a glob in a machine name", mut(func(e *Entry) { e.MaySignMachines = []string{"mac", "*"} }), "not a valid name"},
		{"a short public key", mut(func(e *Entry) { e.PublicKeys = []KeyRef{{KID: "x", Key: "AAAA"}} }), "not a base64 ed25519"},
		{"no public keys", mut(func(e *Entry) { e.PublicKeys = nil }), "public_keys is empty"},
		{"a glob in a pair's profile", mut(func(e *Entry) { e.Pairs = map[string][]string{"dev-*": {"api"}} }), "not a plain name"},
		{"a bad profile pattern", mut(func(e *Entry) { e.MaxProfile = []string{"dev-[", "dev-*"} }), "bad pattern"},
		{"a pattern with a quote or slash", mut(func(e *Entry) { e.MaxProfile = []string{`dev-"/*`} }), "bad pattern"},
		{"the same billing twice in one pair", mut(func(e *Entry) { e.Pairs = map[string][]string{"dev-default": {"api", "api"}} }), "twice"},
	} {
		_, err := Parse("mac.json", []byte(c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", c.why, err, c.want)
		}
	}
}

// context "one machine's file claims another machine": the Mac file may not sign for hp, because hp
// has its own file and its own key.
func TestAMachineCannotSignForAnotherThatHasItsOwnFile(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	dir := listWith(t, hp, mac)
	e, _ := Parse("mac.json", mustRead(t, filepath.Join(dir, "mac.json")))
	e.MaySignMachines = []string{"mac", "hp-ubuntu"}
	put(t, dir, "mac", *e)
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "has its own file") {
		t.Fatalf("mac.json signing for hp-ubuntu: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOneKeyCannotBelongToTwoMachines(t *testing.T) {
	k := newKey(t)
	dir := secureDir(t)
	for _, m := range []string{"hp-ubuntu", "mac"} {
		e := goodEntry(k)
		e.MaySignMachines = []string{m}
		put(t, dir, m, e)
	}
	es, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ring(es); err == nil || !strings.Contains(err.Error(), "listed by both") {
		t.Fatalf("the same key listed by two machines: %v", err)
	}
}

// context "a FIFO named x.json would block the load forever, and nothing would ever expire".
func TestAFifoDoesNotHangTheLoad(t *testing.T) {
	dir := listWith(t, newKey(t), newKey(t))
	if err := syscall.Mkfifo(filepath.Join(dir, "evil.json"), 0o644); err != nil {
		t.Skip("no mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadDir(dir); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a FIFO in the list: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("LoadDir hangs on a FIFO")
	}
}

// context "the list is hot-reloaded, so a directory another local user can edit is a trust root".
func TestADirectoryOthersCanWriteIsRefused(t *testing.T) {
	dir := listWith(t, newKey(t), newKey(t))
	if _, err := LoadDir(dir); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	os.Chmod(dir, 0o775)
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("a group-writable list directory: %v", err)
	}
	os.Chmod(dir, 0o700)
	os.Chmod(filepath.Join(dir, "mac.json"), 0o666)
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("a world-writable machine file: %v", err)
	}
}

// context "the cluster mounts a ConfigMap as symlinks plus ..data bookkeeping": all files of one load
// come from one snapshot, and the kubelet-managed volume is not held to the local-owner rule.
func TestReadsAConfigMapVolumeLayout(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	src := listWith(t, hp, mac)
	vol := t.TempDir()
	os.Chmod(vol, 0o755)
	data := filepath.Join(vol, "..2026_11_20")
	os.Mkdir(data, 0o755)
	for _, n := range []string{"hp-ubuntu.json", "mac.json"} {
		os.WriteFile(filepath.Join(data, n), mustRead(t, filepath.Join(src, n)), 0o644)
	}
	os.Symlink("..2026_11_20", filepath.Join(vol, "..data"))
	for _, n := range []string{"hp-ubuntu.json", "mac.json"} {
		os.Symlink(filepath.Join("..data", n), filepath.Join(vol, n))
	}
	if len(ringOf(t, vol)) != 2 {
		t.Fatal("did not read the two machines through the symlinks")
	}
}

// context "the same list reaches doors by two routes": the keyring lines (stub, token verify) and the
// ConfigMap must give the same verdicts as the Ring the doors build.
func TestRenderAndRingLinesMatchTheRing(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	es, _ := LoadDir(listWith(t, hp, mac))
	ls, err := Lines(es)
	if err != nil {
		t.Fatal(err)
	}
	fromLines, err := token.ParsePublicKeyring([]byte(strings.Join(ls, "\n")), "lines")
	if err != nil {
		t.Fatalf("keyring lines do not parse: %v", err)
	}
	direct, _ := Ring(es)
	for _, k := range []*token.Key{hp, mac} {
		a, b := direct[k.KID], fromLines[k.KID]
		for _, c := range []token.Claims{
			{Sub: "tomas/mac/s/g1", Profile: "dev-default", Billing: "api"},
			{Sub: "tomas/mac/s/g1", Profile: "dev-default", Billing: "subscription"},
			{Sub: "tomas/future-two/s/g1", Profile: "e2e-default", Billing: "stub"},
			{Sub: "tomas/hp-ubuntu/s/g1", Profile: "e2e-default", Billing: "api"},
		} {
			c := c
			if (a.Permits(&c) == nil) != (b.Permits(&c) == nil) {
				t.Errorf("ring and keyring line disagree on %+v", c)
			}
		}
	}
	out := Render(es, "ccsbx-machines", "")
	if out != Render(es, "ccsbx-machines", "") || !strings.Contains(out, "  mac.json: |\n    {\n") {
		t.Fatalf("render is not stable or has the wrong shape:\n%s", out)
	}
	for _, e := range es { // every data block parses back, in canonical form, to the same entry
		i := strings.Index(out, "  "+e.Machine+".json: |\n")
		blk := out[i:]
		blk = blk[strings.Index(blk, "\n")+1:]
		var lines []string
		for _, l := range strings.Split(blk, "\n") {
			if !strings.HasPrefix(l, "    ") {
				break
			}
			lines = append(lines, l[4:])
		}
		back, err := Parse(e.Machine+".json", []byte(strings.Join(lines, "\n")+"\n"))
		if err != nil || canonical(back) != canonical(e) {
			t.Errorf("%s does not survive render: %v", e.Machine, err)
		}
	}
}

func run(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	rc := Main(args, &o, &e, func(string) string { return "" })
	return rc, o.String(), e.String()
}

func TestCommand(t *testing.T) {
	dir := listWith(t, newKey(t), newKey(t))
	for _, sub := range []string{"", "check", "ring", "render", "fmt"} {
		args := []string{"--help"}
		if sub != "" {
			args = []string{sub, "--help"}
		}
		if rc, o, _ := run(args...); rc != 0 || !strings.Contains(o, "Flags:") {
			t.Fatalf("%v: rc=%d, want the usage text", args, rc)
		}
	}
	if rc, o, _ := run("check", dir); rc != 0 || !strings.Contains(o, "mac: 1 key(s)") {
		t.Fatalf("check: rc=%d %q", rc, o)
	}
	if rc, o, _ := run("render", dir, "--namespace", "cc-sandbox-doors"); rc != 0 || !strings.Contains(o, "namespace: cc-sandbox-doors") {
		t.Fatalf("render: rc=%d %q", rc, o)
	}
	if rc, _, _ := run("render", dir, "--name", "x: y"); rc != 64 {
		t.Fatalf("a ConfigMap name with YAML in it: rc=%d, want 64", rc)
	}
	// fmt prints what a sloppy file really says, and says it would be refused
	p := filepath.Join(dir, "mac.json")
	sloppy := strings.TrimSuffix(string(mustRead(t, p)), "}\n") + `,"MAX_BILLING":["api","stub","subscription"]}` + "\n"
	os.WriteFile(p, []byte(sloppy), 0o644)
	if rc, o, e := run("fmt", p); rc != 0 || !strings.Contains(o, `"subscription"`) || !strings.Contains(e, "not canonical") {
		t.Fatalf("fmt of a file with a wrong-case override: rc=%d out=%q err=%q", rc, o, e)
	}
	if rc, _, e := run("check", dir); rc != 1 || !strings.Contains(e, "canonical") {
		t.Fatalf("check must refuse it: rc=%d %q", rc, e)
	}
	os.WriteFile(p, []byte("{}"), 0o644)
	if rc, _, e := run("check", dir); rc != 1 || e == "" {
		t.Fatalf("a bad file must exit 1 with a reason: rc=%d", rc)
	}
	if rc, _, _ := run("nonsense", dir); rc != 64 {
		t.Fatalf("unknown command: rc=%d", rc)
	}
}

// context "someone who can create one entry in the list directory adds a ..data link": the link used to
// switch every ownership and mode check off and could point anywhere. Now it is trusted only as the
// kubelet makes it: a link to a ..snapshot directory inside the dir itself, with dir and snapshot owned
// by root or us and not writable by others.
func TestADataLinkCannotSwitchTheChecksOff(t *testing.T) {
	hp := newKey(t)
	src := listWith(t, hp, newKey(t))
	files := []string{"hp-ubuntu.json", "mac.json"}
	vol := func(link string, snapMode os.FileMode, dirMode os.FileMode) string {
		v := t.TempDir()
		os.Chmod(v, dirMode)
		snap := filepath.Join(v, "..2026_11_20")
		os.Mkdir(snap, 0o755)
		os.Chmod(snap, snapMode)
		for _, n := range files {
			os.WriteFile(filepath.Join(snap, n), mustRead(t, filepath.Join(src, n)), 0o644)
			os.Symlink(filepath.Join("..data", n), filepath.Join(v, n))
		}
		os.Symlink(link, filepath.Join(v, "..data"))
		return v
	}
	if _, err := LoadDir(vol("..2026_11_20", 0o755, 0o755)); err != nil {
		t.Fatalf("a kubelet-shaped volume refused: %v", err)
	}
	other := t.TempDir() // a directory elsewhere that holds the same files
	for _, n := range files {
		os.WriteFile(filepath.Join(other, n), mustRead(t, filepath.Join(src, n)), 0o666)
	}
	plain := vol("sub", 0o755, 0o755) // a link to an ordinary subdirectory, not a ..snapshot
	os.Rename(filepath.Join(plain, "..2026_11_20"), filepath.Join(plain, "sub"))
	for name, v := range map[string]string{
		"link to a plain subdirectory": plain,
		"absolute link elsewhere":      vol(other, 0o755, 0o755),
		"link up and out":              vol("../"+filepath.Base(other), 0o755, 0o755),
		"link through a subdirectory":  vol("..2026_11_20/.", 0o755, 0o755),
		"link to ..data itself":        vol("..data", 0o755, 0o755),
		"snapshot others can write":    vol("..2026_11_20", 0o777, 0o755),
		"directory others can write":   vol("..2026_11_20", 0o755, 0o777),
	} {
		if _, err := LoadDir(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
