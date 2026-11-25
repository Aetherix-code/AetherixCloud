// Package lease implements leases with fencing tokens on top of the
// eventually-consistent store.
//
// The store only offers last-write-wins rows, so a lease is represented as one
// claim row per (lease name, claimant), id "<name>/<owner>". The effective
// holder is a pure function of the live (unexpired) claim rows:
//
//	holder = claim with the highest fencing token; ties broken by the lowest
//	         machine ID.
//
// Acquire: (1) bail if another live holder exists, (2) write our claim with
// fence = max(all fences ever seen)+1, (3) wait Settle for gossip to
// converge (default 2s), (4) re-read and win only if we are the holder. A
// loser deletes its claim. Expired claim rows of the previous holder are kept,
// so fencing tokens stay monotonic across holders.
//
// Fencing: the token returned in Lease.Fence must be attached to every side
// effect performed under the lease (e.g. Replica.fence), so a stale holder's
// writes can be recognised and ignored.
package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aetherix/aetherixcloud/internal/store"
)

var (
	// ErrHeld means another owner holds the lease.
	ErrHeld = errors.New("lease: held by another owner")
	// ErrLost means we no longer hold the lease.
	ErrLost = errors.New("lease: lost")
)

// DefaultSettle is the gossip convergence wait (plan: bounded by ~2s).
const DefaultSettle = 2 * time.Second

// Claim is the persisted claim row.
type Claim struct {
	Name            string `json:"name"`
	Owner           string `json:"owner"`
	Fence           int64  `json:"fence"`
	ExpiresUnixNano int64  `json:"expires"`
}

func (c Claim) live(now time.Time) bool { return now.UnixNano() < c.ExpiresUnixNano }

// Manager creates leases for one machine.
type Manager struct {
	Store  store.Store
	Self   string        // machine ID of the claimant
	Settle time.Duration // wait before reading back a claim
	Now    func() time.Time
}

// NewManager returns a Manager with default settings.
func NewManager(s store.Store, self string) *Manager {
	return &Manager{Store: s, Self: self, Settle: DefaultSettle, Now: time.Now}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func claimID(name, owner string) string { return name + "/" + owner }

func (m *Manager) claims(ctx context.Context, name string) ([]Claim, error) {
	return store.ListJSON[Claim](ctx, m.Store, store.TableLeases, store.Query{Where: map[string]string{"name": name}})
}

// Holder returns the effective live holder of name, or nil.
func (m *Manager) Holder(ctx context.Context, name string) (*Claim, error) {
	cs, err := m.claims(ctx, name)
	if err != nil {
		return nil, err
	}
	return holderOf(cs, m.now()), nil
}

// holderOf is the deterministic holder function (exported behaviour is tested).
func holderOf(cs []Claim, now time.Time) *Claim {
	var live []Claim
	for _, c := range cs {
		if c.live(now) {
			live = append(live, c)
		}
	}
	if len(live) == 0 {
		return nil
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].Fence != live[j].Fence {
			return live[i].Fence > live[j].Fence
		}
		return live[i].Owner < live[j].Owner
	})
	h := live[0]
	return &h
}

func (m *Manager) writeClaim(ctx context.Context, c Claim) error {
	return store.PutJSON(ctx, m.Store, store.TableLeases, claimID(c.Name, c.Owner),
		map[string]string{"name": c.Name, "owner": c.Owner}, c)
}

// Lease is a held lease.
type Lease struct {
	Name  string
	Owner string
	// Fence is the fencing token for this tenure.
	Fence int64

	m       *Manager
	ttl     time.Duration
	expires time.Time
}

// Valid reports whether the lease is unexpired by our local clock. Callers
// must still attach Fence to side effects.
func (l *Lease) Valid() bool { return l.m.now().Before(l.expires) }

// Expires returns the local expiry time.
func (l *Lease) Expires() time.Time { return l.expires }

// Acquire tries once to take (or re-confirm) the lease.
func (m *Manager) Acquire(ctx context.Context, name string, ttl time.Duration) (*Lease, error) {
	cs, err := m.claims(ctx, name)
	if err != nil {
		return nil, err
	}
	now := m.now()
	if h := holderOf(cs, now); h != nil {
		if h.Owner != m.Self {
			return nil, fmt.Errorf("%w: %s (fence %d)", ErrHeld, h.Owner, h.Fence)
		}
		// We already hold it: extend.
		l := &Lease{Name: name, Owner: m.Self, Fence: h.Fence, m: m, ttl: ttl}
		if err := l.Renew(ctx); err != nil {
			return nil, err
		}
		return l, nil
	}
	var maxFence int64
	for _, c := range cs {
		if c.Fence > maxFence {
			maxFence = c.Fence
		}
	}
	mine := Claim{Name: name, Owner: m.Self, Fence: maxFence + 1, ExpiresUnixNano: now.Add(ttl).UnixNano()}
	if err := m.writeClaim(ctx, mine); err != nil {
		return nil, err
	}
	if m.Settle > 0 {
		select {
		case <-time.After(m.Settle):
		case <-ctx.Done():
			_ = m.Store.Delete(context.WithoutCancel(ctx), store.TableLeases, claimID(name, m.Self))
			return nil, ctx.Err()
		}
	}
	cs, err = m.claims(ctx, name)
	if err != nil {
		return nil, err
	}
	h := holderOf(cs, m.now())
	if h == nil || h.Owner != m.Self || h.Fence != mine.Fence {
		_ = m.Store.Delete(ctx, store.TableLeases, claimID(name, m.Self))
		if h != nil {
			return nil, fmt.Errorf("%w: %s (fence %d)", ErrHeld, h.Owner, h.Fence)
		}
		return nil, ErrLost
	}
	return &Lease{Name: name, Owner: m.Self, Fence: mine.Fence, m: m, ttl: ttl, expires: time.Unix(0, mine.ExpiresUnixNano)}, nil
}

// Renew extends the lease; returns ErrLost if someone else took over.
func (l *Lease) Renew(ctx context.Context) error {
	cs, err := l.m.claims(ctx, l.Name)
	if err != nil {
		return err
	}
	now := l.m.now()
	h := holderOf(cs, now)
	// Allow renewing an expired-but-unclaimed tenure only if nobody else holds.
	if h != nil && (h.Owner != l.Owner || h.Fence != l.Fence) {
		return ErrLost
	}
	if h == nil {
		for _, c := range cs { // someone with a higher fence expired meanwhile?
			if c.Fence > l.Fence {
				return ErrLost
			}
		}
	}
	exp := now.Add(l.ttl)
	if err := l.m.writeClaim(ctx, Claim{Name: l.Name, Owner: l.Owner, Fence: l.Fence, ExpiresUnixNano: exp.UnixNano()}); err != nil {
		return err
	}
	l.expires = exp
	return nil
}

// Release gives the lease up immediately (the claim row is kept, expired, to
// preserve fencing monotonicity).
func (l *Lease) Release(ctx context.Context) error {
	l.expires = time.Time{}
	return l.m.writeClaim(ctx, Claim{Name: l.Name, Owner: l.Owner, Fence: l.Fence, ExpiresUnixNano: 0})
}

// Run keeps trying to hold the lease and, while held, runs fn with a context
// that is cancelled when the lease is lost. It returns when ctx is done.
// fn is restarted every time the lease is (re)acquired.
func (m *Manager) Run(ctx context.Context, name string, ttl time.Duration, fn func(ctx context.Context, l *Lease)) {
	retry := ttl / 3
	if retry <= 0 {
		retry = time.Second
	}
	for ctx.Err() == nil {
		l, err := m.Acquire(ctx, name, ttl)
		if err != nil {
			select {
			case <-time.After(retry):
			case <-ctx.Done():
			}
			continue
		}
		hctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); fn(hctx, l) }()
		t := time.NewTicker(retry)
	hold:
		for {
			select {
			case <-ctx.Done():
				break hold
			case <-done:
				break hold
			case <-t.C:
				if err := l.Renew(ctx); err != nil && (errors.Is(err, ErrLost) || !l.Valid()) {
					break hold
				}
			}
		}
		t.Stop()
		cancel()
		<-done
		_ = l.Release(context.WithoutCancel(ctx))
	}
}

// compile-time check that Claim round-trips.
var _ = json.Marshal
