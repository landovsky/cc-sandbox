// revoker.go ends a docker generation's token at the doors. When a generation's outbox has been drained for
// the last time, hostd asks the signer to sign the revoke (hostd holds no key) and posts it to every door
// in --revoke-door, retrying a few times in the background; if it still fails it says which sid and generation
// to revoke by hand. It never blocks the scan.
package main

import (
	"context"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/revoke"
	"github.com/landovsky/cc-sandbox/internal/signer"
	"github.com/landovsky/cc-sandbox/internal/token"
)

// revokeBackoff is the wait before each retry; a variable so a test can shorten it.
var revokeBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

func generationEndRevoker(sock, doors string, logf func(string, ...any)) func(sid string, gen int, owner, machine string) {
	var urls []string
	for _, u := range strings.Split(doors, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	if sock == "" || len(urls) == 0 {
		return nil
	}
	return func(sid string, gen int, owner, machine string) {
		go func() {
			b := token.RevokeBody{Owner: owner, Machine: machine, Sid: sid, Gen: gen}
			var err error
			for i := 0; ; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				var hdr string
				var body []byte
				if hdr, body, err = (signer.Client{Socket: sock}).SignRevoke(ctx, b); err == nil {
					err = revoke.Doors{URLs: urls}.Post(ctx, hdr, body)
				}
				cancel()
				if err == nil {
					logf("revoke: %s generation %d revoked at the doors", sid, gen)
					return
				}
				if i >= len(revokeBackoff) {
					logf("revoke: %s generation %d NOT revoked (%v): revoke sid %s gen %d by hand", sid, gen, err, sid, gen)
					return
				}
				time.Sleep(revokeBackoff[i])
			}
		}()
	}
}
