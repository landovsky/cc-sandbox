// machines_test.go checks the trusted-key list (M9) the way a door meets it: files in, a verdict on
// real tokens out. Each context names the problem the rule exists to stop.
package machines

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func b64(k *token.Key) string { return base64.StdEncoding.EncodeToString(k.Public) }

func put(t *testing.T, dir, machine string, e Entry) {
	t.Helper()
	e.Machine = machine
	b, _ := json.Marshal(e)
	if err := os.WriteFile(filepath.Join(dir, machine+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// listWith writes hp (which also signs for future-two) and a Mac whose ceiling is api and stub.
func listWith(t *testing.T, hp, mac *token.Key) string {
	dir := t.TempDir()
	put(t, dir, "hp-ubuntu", Entry{Owners: []string{"tomas"}, PublicKeys: []string{b64(hp)},
		MaySignMachines: []string{"hp-ubuntu", "future-two"}, MaxProfile: []string{"dev-*", "e2e-*"},
		MaxBilling: []string{"api", "subscription", "stub"},
		Pairs:      map[string][]string{"dev-default": {"api", "subscription", "stub"}, "e2e-default": {"stub"}}})
	put(t, dir, "mac", Entry{Owners: []string{"tomas"}, PublicKeys: []string{b64(mac)},
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

// context "a Mac key is stolen (T:key-signs-foreign-machine, T:mac-key-above-mac-max)": the list must
// let a door refuse what the thief can make with it, not only what the real owner never would.
func TestTheListLimitsAStolenKey(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	d := newDoor(t, ringOf(t, listWith(t, hp, mac)))
	if err := d.call(mac, "tomas/mac/sM1/g1", "dev-default", "api"); err != nil {
		t.Fatalf("allow twin: the Mac key, its own machine, api: %v", err)
	}
	if err := d.call(hp, "tomas/future-two/sF1/g1", "dev-default", "subscription"); err != nil {
		t.Fatalf("allow twin: hp signing for future-two: %v", err)
	}
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
}

// context "a key is removed from the list (rotation or theft)": the door must stop trusting it within
// two minutes without a restart, and a broken list must not keep it alive for long.
func TestRemovedKeyIsRefusedWithinTwoMinutes(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	dir := listWith(t, hp, mac)
	now := t0
	r := clocked(t, dir, &now)
	if def, _ := NewReloader(dir); def.Every > 30*time.Second || def.MaxStale <= def.Every {
		t.Fatalf("default poll %s / stale limit %s: a removed key must be refused within 2 minutes", def.Every, def.MaxStale)
	}
	d := newDoor(t, r.Ring())
	apply := func() { d.v.SetRing(r.Ring()) }
	if err := d.call(mac, "tomas/mac/sM1/g1", "dev-default", "api"); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	os.Remove(filepath.Join(dir, "mac.json"))
	now = now.Add(r.Every)
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	apply()
	if err := d.call(mac, "tomas/mac/sM1/g2", "dev-default", "api"); err == nil {
		t.Fatal("the removed Mac key still verifies one poll later")
	}
	if err := d.call(hp, "tomas/hp-ubuntu/sH1/g1", "dev-default", "api"); err != nil {
		t.Fatalf("allow twin: removing the Mac key broke hp: %v", err)
	}
}

func TestABrokenListKeepsTheLastGoodOneOnlyBriefly(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	dir := listWith(t, hp, mac)
	now := t0
	r := clocked(t, dir, &now)
	r.MaxStale = 3 * time.Minute
	os.WriteFile(filepath.Join(dir, "mac.json"), []byte("{not json"), 0o644)
	now = now.Add(time.Minute)
	if err := r.Refresh(); err == nil || len(r.Ring()) != 2 {
		t.Fatalf("one minute into a bad list: err=%v keys=%d, want the last good ring (2 keys)", err, len(r.Ring()))
	}
	now = now.Add(3 * time.Minute)
	r.Refresh()
	if len(r.Ring()) != 0 {
		t.Fatalf("a list bad for longer than MaxStale still trusts %d keys", len(r.Ring()))
	}
	put(t, dir, "mac", Entry{Owners: []string{"tomas"}, PublicKeys: []string{b64(mac)}, MaySignMachines: []string{"mac"},
		MaxProfile: []string{"dev-*"}, MaxBilling: []string{"api"}, Pairs: map[string][]string{"dev-default": {"api"}}})
	if err := r.Refresh(); err != nil || len(r.Ring()) != 2 {
		t.Fatalf("after the fix: err=%v keys=%d, want it to recover", err, len(r.Ring()))
	}
}

// clocked is NewReloader with a test clock set before the first load, so lastGood is on that clock.
func clocked(t *testing.T, dir string, now *time.Time) *Reloader {
	t.Helper()
	r := &Reloader{Dir: dir, Every: 15 * time.Second, MaxStale: 5 * time.Minute, Now: func() time.Time { return *now }}
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFirstLoadFailureStopsTheDoor(t *testing.T) {
	if _, err := NewReloader(t.TempDir()); err == nil {
		t.Fatal("an empty list started a door")
	}
	if _, err := NewReloader(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing list started a door")
	}
}

// context "a typo in a limit must not read as no limit": every refusal below is a file that would
// widen what a key may sign if it were read leniently.
func TestFilesThatWouldWidenAKeyAreRefused(t *testing.T) {
	k := newKey(t)
	good := Entry{Machine: "mac", Owners: []string{"tomas"}, PublicKeys: []string{b64(k)}, MaySignMachines: []string{"mac"},
		MaxProfile: []string{"dev-*"}, MaxBilling: []string{"api", "stub"}, Pairs: map[string][]string{"dev-default": {"api"}}}
	enc := func(e Entry) string { b, _ := json.Marshal(e); return string(b) }
	if _, err := Parse("mac.json", []byte(enc(good))); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	mut := func(f func(e *Entry)) string { e := good; f(&e); return enc(e) }
	cases := map[string]string{
		"unknown field (an extra limit nothing reads)": strings.Replace(enc(good), `{`, `{"max_spend":1,`, 1),
		"data after the object":                        enc(good) + `{}`,
		"machine differs from the file name":           mut(func(e *Entry) { e.Machine = "hp-ubuntu" }),
		"no pairs (a ceiling alone)":                   mut(func(e *Entry) { e.Pairs = nil }),
		"pair profile outside max_profile":             mut(func(e *Entry) { e.Pairs = map[string][]string{"prod": {"api"}} }),
		"pair billing outside max_billing":             mut(func(e *Entry) { e.Pairs = map[string][]string{"dev-default": {"subscription"}} }),
		"a fourth billing":                             mut(func(e *Entry) { e.MaxBilling = []string{"free"} }),
		"empty owners":                                 mut(func(e *Entry) { e.Owners = nil }),
		"empty may_sign_machines":                      mut(func(e *Entry) { e.MaySignMachines = nil }),
		"a glob in a machine name":                     mut(func(e *Entry) { e.MaySignMachines = []string{"*"} }),
		"a short public key":                           mut(func(e *Entry) { e.PublicKeys = []string{"AAAA"} }),
		"no public keys":                               mut(func(e *Entry) { e.PublicKeys = nil }),
		"a glob in a pair's profile":                   mut(func(e *Entry) { e.Pairs = map[string][]string{"dev-*": {"api"}} }),
		"a bad profile pattern":                        mut(func(e *Entry) { e.MaxProfile = []string{"dev-[", "dev-*"} }),
		"the same billing twice in one pair":           mut(func(e *Entry) { e.Pairs = map[string][]string{"dev-default": {"api", "api"}} }),
	}
	for why, body := range cases {
		if _, err := Parse("mac.json", []byte(body)); err == nil {
			t.Errorf("%s: accepted", why)
		}
	}
}

func TestOneKeyCannotBelongToTwoMachines(t *testing.T) {
	k := newKey(t)
	dir := t.TempDir()
	for _, m := range []string{"hp-ubuntu", "mac"} {
		put(t, dir, m, Entry{Owners: []string{"tomas"}, PublicKeys: []string{b64(k)}, MaySignMachines: []string{m},
			MaxProfile: []string{"*"}, MaxBilling: []string{"api"}, Pairs: map[string][]string{"dev-default": {"api"}}})
	}
	es, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ring(es); err == nil {
		t.Fatal("the same key listed by two machines")
	}
}

// context "the cluster mounts a ConfigMap as symlinks plus ..data bookkeeping".
func TestReadsAConfigMapVolumeLayout(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	src := listWith(t, hp, mac)
	vol := t.TempDir()
	data := filepath.Join(vol, "..2026_11_20")
	os.Mkdir(data, 0o755)
	for _, n := range []string{"hp-ubuntu.json", "mac.json"} {
		b, _ := os.ReadFile(filepath.Join(src, n))
		os.WriteFile(filepath.Join(data, n), b, 0o644)
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
	// every data block parses back to the same entry
	for _, e := range es {
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
		back, err := Parse(e.Machine+".json", []byte(strings.Join(lines, "\n")))
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
	if rc, o, _ := run("--help"); rc != 0 || !strings.Contains(o, "Flags:") {
		t.Fatalf("--help: rc=%d", rc)
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
	os.WriteFile(filepath.Join(dir, "mac.json"), []byte("{}"), 0o644)
	if rc, _, e := run("check", dir); rc != 1 || e == "" {
		t.Fatalf("a bad file must exit 1 with a reason: rc=%d", rc)
	}
	if rc, _, _ := run("nonsense", dir); rc != 64 {
		t.Fatalf("unknown command: rc=%d", rc)
	}
}
