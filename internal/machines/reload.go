// reload.go keeps a running door's keyring in step with the trusted-key list. It exists so removing
// a machine's key (a rotation, or a theft) reaches every door within minutes with no restart, and
// so a list that goes bad can't leave a removed key alive for long: the last good ring is kept for
// MaxStale, then the door trusts nothing (every token 401).
package machines

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// DefaultEvery and DefaultMaxStale: a removed key must be refused within 2 minutes (plan M9). The
// kubelet takes up to 60 s to update a ConfigMap volume, so the poll plus the stale limit have to fit
// in the other 60 s even when the removal arrives together with a broken list.
const (
	DefaultEvery    = 15 * time.Second
	DefaultMaxStale = 45 * time.Second
)

// Reloader re-reads Dir every Every. The ring it hands out goes empty once the last good read is
// MaxStale old, judged when Ring() is called, so a poll that hangs or a goroutine that died still
// ends trust in a removed key.
type Reloader struct {
	Dir      string
	Every    time.Duration
	MaxStale time.Duration
	Logf     func(format string, a ...any)
	Now      func() time.Time // tests

	mu       sync.Mutex
	ring     token.Keyring
	lastGood time.Time
	failing  bool
}

// NewReloader loads once; an error means the door must not start (fail closed).
func NewReloader(dir string) (*Reloader, error) {
	r := &Reloader{Dir: dir, Every: DefaultEvery, MaxStale: DefaultMaxStale}
	if err := r.Refresh(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reloader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Ring is the keyring in force now: empty when the last good read is MaxStale old or more.
func (r *Reloader) Ring() token.Keyring {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.now().Sub(r.lastGood) >= r.MaxStale {
		return token.Keyring{}
	}
	return r.ring
}

// Refresh reads the list once. On a failure the last good ring stays until MaxStale has passed, then
// the ring becomes empty. It returns the read error either way.
func (r *Reloader) Refresh() error {
	es, err := LoadDir(r.Dir)
	var ring token.Keyring
	if err == nil {
		ring, err = Ring(es)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if err == nil {
		if r.failing && r.Logf != nil {
			r.Logf("machines: trusted-key list readable again")
		}
		r.ring, r.lastGood, r.failing = ring, now, false
		return nil
	}
	if r.ring == nil && r.lastGood.IsZero() {
		return err // the first load: nothing to fall back on
	}
	if r.Logf != nil {
		r.Logf("machines: trusted-key list unreadable (%v); keeping the last good one until %s", err, r.lastGood.Add(r.MaxStale).Format(time.RFC3339))
	}
	r.failing = true
	if now.Sub(r.lastGood) >= r.MaxStale && r.Logf != nil {
		r.Logf("machines: trusted-key list stale for %s: trusting no key", r.MaxStale)
	}
	return err
}

// Run refreshes every Every until ctx ends. A door reads the result through Ring (for a Verifier:
// SetRingSource(r.Ring)), so nothing is pushed anywhere and a stalled Run cannot keep an old ring alive.
// apply, when not nil, also gets each ring (for callers that need a push).
func (r *Reloader) Run(ctx context.Context, apply func(token.Keyring)) error {
	if r.Every <= 0 || r.MaxStale < r.Every {
		return errors.New("machines: Every must be positive and MaxStale at least Every")
	}
	if apply == nil {
		apply = func(token.Keyring) {}
	}
	apply(r.Ring())
	t := time.NewTicker(r.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.Refresh()
			apply(r.Ring())
		}
	}
}
