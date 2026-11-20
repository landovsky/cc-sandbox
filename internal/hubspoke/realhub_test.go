// The B1 acceptance against the REAL session-bus hub (dotfiles branch n1-c), on a spare port: hostd's
// spoke registers a fenced session and receives a test frame. Why a separate file: it needs a hub
// container, so it only runs from tests/hub-spoke-test.sh (which starts one and sets the variables);
// under `go test` alone it skips.
package hubspoke

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
)

func TestRealHubRegistersAFencedSessionAndReceivesATestFrame(t *testing.T) {
	url, admin, spokeTok := os.Getenv("CCSBX_REAL_HUB_URL"), os.Getenv("CCSBX_REAL_HUB_ADMIN"), os.Getenv("CCSBX_REAL_HUB_SPOKE_TOKEN")
	if url == "" || admin == "" || spokeTok == "" {
		t.Skip("needs a test hub: run tests/hub-spoke-test.sh")
	}
	const machine, sid = "m1", "s0realhub1"
	var mu sync.Mutex
	var got []string
	sp := New(Config{
		Client:  &hubc.Client{URL: url, Token: func() (string, error) { return spokeTok, nil }},
		Machine: machine, Name: "ccsbx", TTL: 30, PollWait: 2 * time.Second,
		Entries: func() []map[string]any {
			return []map[string]any{{"key": "ccsbx:" + sid, "uid": sid, "label": "real-hub-test", "machine": machine, "fenced": true,
				"downlink": "spoke:" + machine + "/ccsbx", "attach": "ccsbx:" + sid, "state": "waiting"}}
		},
		OnDeliver: func(_ context.Context, _ hubc.Frame, d hubc.Deliver) Result {
			mu.Lock()
			got = append(got, d.Text)
			mu.Unlock()
			return Result{OK: true, Confirmed: true}
		},
		Logf: t.Logf,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sp.Run(ctx)

	var sessions []map[string]any
	for i := 0; i < 100 && len(sessions) == 0; i++ {
		req, _ := http.NewRequest("GET", url+"/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var all []map[string]any
			json.Unmarshal(b, &all)
			for _, e := range all {
				if e["label"] == "real-hub-test" {
					sessions = append(sessions, e)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(sessions) != 1 || sessions[0]["fenced"] != true || sessions[0]["pane"] != nil || sessions[0]["attach"] != "ccsbx:"+sid {
		t.Fatalf("the fenced session is not registered as fenced with no pane: %v", sessions)
	}
	c := &hubc.Client{URL: url, Token: func() (string, error) { return admin, nil }}
	ctx2, cancel2 := context.WithTimeout(ctx, 15*time.Second)
	defer cancel2()
	res, err := c.Send(ctx2, hubc.SendReq{To: sessions[0]["id"].(string), Text: "hello hostd"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered != 1 || res.Matched != 1 {
		t.Fatalf("the hub's deliver answer: %+v (the spoke's ack was not relayed)", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "hello hostd") {
		t.Fatalf("the spoke got %v", got)
	}
}
