// Test of the direct-mode start's cross-machine label check (M2c review M1): `ccsbx start` in direct mode runs the
// start in its own process, so without this it would never ask the peers and a second generation of a label that
// lives elsewhere would launch (on the Mac, where direct mode is the usual setup).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/remotestart"
	"github.com/landovsky/cc-sandbox/internal/start"
)

func TestADirectModeStartAsksThePeersAndRefusesWhatLivesElsewhere(t *testing.T) {
	state := t.TempDir()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(remotestart.Answer{PeerSet: "S", Live: []remotestart.View{{Machine: "hp-ubuntu", Sid: "s0there", State: "running"}}})
	}))
	defer peer.Close()
	pub, _ := json.Marshal(remotestart.Published{Machine: "mac-tomas", Peers: []remotestart.Peer{{Name: "hp-ubuntu", URL: peer.URL}}, PeerSet: "S", AskTimeoutMs: 2000})
	os.MkdirAll(filepath.Join(state, "remote"), 0o700)
	os.WriteFile(filepath.Join(state, "remote", "peers.json"), pub, 0o644)
	d := &start.Daemon{StateDir: state, Machine: "mac-tomas", Launcher: start.NoLauncher{}}
	spec := &start.LaunchSpec{V: 1, RequestID: "r1", Label: "cc-sandbox:x", Machine: "mac-tomas"}
	_, err := guardedStart(d, spec)
	if r, ok := err.(*start.Refusal); !ok || r.Code != start.AlreadyRunning || r.Machine != "hp-ubuntu" {
		t.Fatalf("a direct start of a label that is live on a peer: %v", err)
	}
	// a peers file that exists but cannot be read must refuse, not skip the check
	os.WriteFile(filepath.Join(state, "remote", "peers.json"), []byte("{broken"), 0o644)
	if _, err := guardedStart(d, spec); err == nil {
		t.Fatal("an unreadable peer list was treated as no check")
	}
	if _, ok := err.(*start.Refusal); !ok {
		t.Fatalf("%T", err)
	}
}
