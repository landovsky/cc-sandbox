// Tests of the tailnet listener's caller classes (M2a, T:daemon-authz). Why: the whole point is that a
// request's authority comes from what Tailscale says about the peer's address and from nothing the
// request carries, so each case is a way to claim more than you are: another login, an unlisted device,
// a tagged node, a forged identity header, an unanswered WhoIs, a wildcard listener.
package daemonapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/verbq"
	"github.com/landovsky/cc-sandbox/internal/whois"
)

// fakeWho answers by remote IP.
type fakeWho struct {
	ids   map[string]whois.Identity
	calls int
}

func (f *fakeWho) WhoIs(_ context.Context, addr string) (whois.Identity, error) {
	f.calls++
	host := strings.Split(addr, ":")[0]
	if id, ok := f.ids[host]; ok {
		return id, nil
	}
	return whois.Identity{}, errors.New("whois: no such peer")
}

func id(login, short string, tags ...string) whois.Identity {
	return whois.Identity{Login: login, Device: short + ".example.ts.net", Short: short, Tags: tags}
}

var cc = ClassConfig{OwnerLogin: "tomas@example.com", OwnerDevices: []string{"mac-tomas", "hp-ubuntu"}, PeerDevices: []string{"future-two"}, PeerTags: []string{"tag:ccsbx"}}

func tailnetRig(t *testing.T) (http.Handler, *fakeWho) {
	t.Helper()
	j, _ := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	q, _ := verbq.Open(t.TempDir())
	t.Cleanup(func() { j.Close(); q.Close() })
	who := &fakeWho{ids: map[string]whois.Identity{
		"100.64.0.1": id("tomas@example.com", "mac-tomas"),              // the owner on a listed device
		"100.64.0.2": id("tomas@example.com", "old-laptop"),             // the owner's login on an UNLISTED device
		"100.64.0.3": id("mallory@example.com", "mac-tomas"),            // someone else's login on a listed device name
		"100.64.0.4": id("tagged-devices", "future-two", "tag:ccsbx"),   // a listed daemon
		"100.64.0.5": id("tagged-devices", "stranger-pod", "tag:ccsbx"), // carries the listed tag
		"100.64.0.6": id("tagged-devices", "rogue", "tag:other"),        // some other tag
		"100.64.0.7": id("tomas@example.com", "hp-ubuntu", "tag:ccsbx"), // the owner's login but TAGGED: not a person
	}}
	s := &Server{J: j, Q: q, Info: map[string]any{"machine": "hp-ubuntu"}}
	return s.TailnetHandler(who, cc), who
}

func tcall(h http.Handler, from, method, path string, hdr ...string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.RemoteAddr = from + ":50000"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// T:daemon-authz, allow: the owner on a listed device and a listed daemon each get what their class gets.
func TestOwnerAndPeerGetTheirClassAllow(t *testing.T) {
	h, _ := tailnetRig(t)
	if code, body := tcall(h, "100.64.0.1", "GET", "/v1/info"); code != 200 || !strings.Contains(body, `"caller_class":"owner"`) {
		t.Fatalf("owner: %d %s", code, body)
	}
	if code, body := tcall(h, "100.64.0.1", "GET", "/v1/events?after=0"); code != 200 {
		t.Fatalf("owner events: %d %s", code, body)
	}
	if code, body := tcall(h, "100.64.0.4", "GET", "/v1/info"); code != 200 || !strings.Contains(body, `"caller_class":"peer"`) {
		t.Fatalf("peer: %d %s", code, body)
	}
	if code, _ := tcall(h, "100.64.0.5", "GET", "/v1/info"); code != 200 {
		t.Fatalf("a node carrying the listed tag is a peer: %d", code)
	}
}

// T:daemon-authz, block: every way to claim more than you are.
func TestEveryOtherCallerIsRefusedBlock(t *testing.T) {
	h, _ := tailnetRig(t)
	for name, from := range map[string]string{
		"the owner's login on an unlisted device": "100.64.0.2",
		"another login on a listed device's name": "100.64.0.3",
		"a tagged node with another tag":          "100.64.0.6",
		"the owner's login on a TAGGED node":      "100.64.0.7",
		"an address Tailscale does not know":      "100.64.9.9",
		"a LAN address":                           "192.168.1.5",
	} {
		if name == "the owner's login on a TAGGED node" {
			// tagged with the listed tag: it is a peer, never the owner
			if code, body := tcall(h, from, "GET", "/v1/info"); code != 200 || !strings.Contains(body, `"caller_class":"peer"`) {
				t.Errorf("%s: %d %s", name, code, body)
			}
			if code, _ := tcall(h, from, "POST", "/v1/cockpit/lease?realm=production"); code != 403 {
				t.Errorf("%s: owner operation: %d", name, code)
			}
			continue
		}
		if code, _ := tcall(h, from, "GET", "/v1/info"); code != 403 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

// context: a peer daemon is not the owner: it may not deliver, take the cockpit lease, read the journal or
// prompt/replace a session. Even if a route forgot to check, the peer handler is not the owner handler.
func TestAPeerCannotDoOwnerThingsBlock(t *testing.T) {
	h, _ := tailnetRig(t)
	for _, c := range [][2]string{
		{"POST", "/v1/sessions/s0x/deliver"}, {"POST", "/v1/sessions/s0x/prompt"}, {"POST", "/v1/sessions/s0x/replace"},
		{"POST", "/v1/cockpit/lease?realm=production"}, {"GET", "/v1/events?after=0"}, {"GET", "/v1/cockpit/status?realm=production"},
		{"POST", "/v1/cursors/production"}, {"GET", "/v1/events/stream"}, {"POST", "/v1/start"},
	} {
		if code, _ := tcall(h, "100.64.0.4", c[0], c[1]); code != 403 {
			t.Errorf("%s %s as a peer: %d", c[0], c[1], code)
		}
	}
}

// context: session start needs the caller's uid, which a tailnet caller does not have; it stays on the
// local socket even for the owner (M2c adds the remote form with its own identity and rate limits).
func TestStartIsNotServedOnTheTailnetEvenToTheOwner(t *testing.T) {
	h, _ := tailnetRig(t)
	if code, _ := tcall(h, "100.64.0.1", "POST", "/v1/start"); code != 403 {
		t.Fatalf("%d", code)
	}
}

// context: identity headers are what an operator proxy in front of a door sets; here they are the caller's
// own words. Claiming to be the owner in one changes nothing.
func TestForgedIdentityHeadersAreIgnoredBlock(t *testing.T) {
	h, _ := tailnetRig(t)
	if code, _ := tcall(h, "100.64.0.6", "GET", "/v1/info", "Tailscale-User-Login", "tomas@example.com", "Tailscale-User-Name", "Tomas", "X-Forwarded-For", "100.64.0.1"); code != 403 {
		t.Fatalf("a forged Tailscale-* header or X-Forwarded-For was believed: %d", code)
	}
	if code, _ := tcall(h, "100.64.0.4", "POST", "/v1/sessions/s0x/deliver", "Tailscale-User-Login", "tomas@example.com"); code != 403 {
		t.Fatalf("a peer with a forged owner header: %d", code)
	}
}

// context: WhoIs unanswered (tailscaled down, the CLI missing, a timeout) is "unknown", never "owner".
func TestAFailedWhoisIsForbidden(t *testing.T) {
	h, who := tailnetRig(t)
	who.ids = nil
	if code, _ := tcall(h, "100.64.0.1", "GET", "/v1/info"); code != 403 {
		t.Fatalf("%d", code)
	}
}

// context: nothing may listen on a wildcard or LAN address: "nothing live before the ACL".
func TestTailnetListenerBindsOnlyATailnetAddress(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:7480", "[::]:7480", ":7480", "192.168.1.5:7480", "127.0.0.1:7480", "10.0.0.1:7480", "100.64.0.1", "100.64.0.1:0", "nonsense", "100.128.0.1:7480"} {
		if l, err := ListenTailnet(bad); err == nil {
			l.Close()
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestClassifyMatchesShortAndFullNamesCaseInsensitively(t *testing.T) {
	c := ClassConfig{OwnerLogin: "Tomas@Example.com", OwnerDevices: []string{"HP-Ubuntu.example.ts.net."}}
	if got := c.Classify(whois.Identity{Login: "tomas@example.com", Device: "hp-ubuntu.example.ts.net", Short: "hp-ubuntu"}); got != ClassOwner {
		t.Fatalf("%v", got)
	}
	if got := (ClassConfig{}).Classify(whois.Identity{Login: "tomas@example.com", Device: "x.ts.net", Short: "x"}); got != ClassNone {
		t.Fatalf("an empty config must classify nobody: %v", got)
	}
}
