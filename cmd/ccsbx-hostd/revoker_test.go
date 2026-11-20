package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// a signer that answers every revoke with a fixed header, and records what it was asked
func fakeSigner(t *testing.T, fail *bool) (string, *[]token.RevokeBody) {
	t.Helper()
	d, _ := os.MkdirTemp("", "sg")
	t.Cleanup(func() { os.RemoveAll(d) })
	sock := filepath.Join(d, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var mu sync.Mutex
	var got []token.RevokeBody
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req struct{ Revoke token.RevokeBody }
				json.NewDecoder(c).Decode(&req)
				mu.Lock()
				got = append(got, req.Revoke)
				mu.Unlock()
				if fail != nil && *fail {
					json.NewEncoder(c).Encode(map[string]string{"error": "refused"})
					return
				}
				b, _ := json.Marshal(req.Revoke)
				json.NewEncoder(c).Encode(map[string]string{"revoke": "kid.1.sig", "body": string(b)})
			}()
		}
	}()
	return sock, &got
}

func TestAGenerationThatEndsIsRevokedAtTheDoors(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, r.URL.Path+" "+r.Header.Get(token.RevokeHeader)+" "+string(b))
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer door.Close()
	sock, asked := fakeSigner(t, nil)
	var logs []string
	var lm sync.Mutex
	logf := func(f string, a ...any) {
		lm.Lock()
		logs = append(logs, strings.TrimSpace(sprintf(f, a...)))
		lm.Unlock()
	}
	fn := generationEndRevoker(sock, door.URL, logf)
	if fn == nil {
		t.Fatal("no revoker")
	}
	fn("s0abcdefgh", 2, "tomas", "hp-ubuntu")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(posts) == 1 })
	if len(*asked) != 1 || (*asked)[0] != (token.RevokeBody{Owner: "tomas", Machine: "hp-ubuntu", Sid: "s0abcdefgh", Gen: 2}) {
		t.Errorf("the signer was asked %+v", *asked)
	}
	if !strings.HasPrefix(posts[0], token.RevokePath+" kid.1.sig {") || !strings.Contains(posts[0], `"gen":2`) {
		t.Errorf("%q", posts[0])
	}
}

func TestARevokeThatNeverWorksSaysWhatToRevokeByHand(t *testing.T) {
	old := revokeBackoff
	revokeBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { revokeBackoff = old }()
	fail := true
	sock, asked := fakeSigner(t, &fail)
	var logs []string
	var lm sync.Mutex
	logf := func(f string, a ...any) { lm.Lock(); logs = append(logs, sprintf(f, a...)); lm.Unlock() }
	generationEndRevoker(sock, "http://127.0.0.1:1", logf)("s0abcdefgh", 3, "tomas", "hp-ubuntu")
	waitFor(t, func() bool { lm.Lock(); defer lm.Unlock(); return len(logs) == 1 })
	if !strings.Contains(logs[0], "NOT revoked") || !strings.Contains(logs[0], "revoke sid s0abcdefgh gen 3 by hand") || len(*asked) != 3 {
		t.Errorf("%q (asked %d times)", logs, len(*asked))
	}
}

func TestNoRevokerWithoutASignerAndDoors(t *testing.T) {
	if generationEndRevoker("", "http://x:1", nil) != nil || generationEndRevoker("/s", "", nil) != nil || generationEndRevoker("/s", " , ", nil) != nil {
		t.Error("a revoker was made with nothing to revoke through")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func sprintf(f string, a ...any) string {
	return strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(f, a...), "\n", " "))
}
