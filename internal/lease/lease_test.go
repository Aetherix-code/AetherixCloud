package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func mgr(s store.Store, id string, c *clock, settle time.Duration) *Manager {
	return &Manager{Store: s, Self: id, Settle: settle, Now: c.now}
}

func TestHolderTieBreak(t *testing.T) {
	now := time.Unix(1000, 0)
	exp := now.Add(time.Minute).UnixNano()
	h := holderOf([]Claim{
		{Owner: "m3", Fence: 5, ExpiresUnixNano: exp},
		{Owner: "m1", Fence: 5, ExpiresUnixNano: exp},
		{Owner: "m2", Fence: 4, ExpiresUnixNano: exp},
		{Owner: "m0", Fence: 9, ExpiresUnixNano: now.Add(-time.Second).UnixNano()}, // expired
	}, now)
	require.NotNil(t, h)
	require.Equal(t, "m1", h.Owner) // highest live fence, lowest ID
	require.Nil(t, holderOf(nil, now))
}

func TestAcquireExclusiveAndFenceMonotonic(t *testing.T) {
	s := store.NewMemory()
	c := &clock{t: time.Unix(1000, 0)}
	ctx := context.Background()
	a, b := mgr(s, "a", c, 0), mgr(s, "b", c, 0)

	la, err := a.Acquire(ctx, "build", time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), la.Fence)

	_, err = b.Acquire(ctx, "build", time.Minute)
	require.True(t, errors.Is(err, ErrHeld))

	// a is renewed idempotently by Acquire.
	la2, err := a.Acquire(ctx, "build", time.Minute)
	require.NoError(t, err)
	require.Equal(t, la.Fence, la2.Fence)

	// expiry hands over with a higher fence
	c.add(2 * time.Minute)
	require.False(t, la.Valid())
	lb, err := b.Acquire(ctx, "build", time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(2), lb.Fence)

	// stale holder cannot renew
	require.ErrorIs(t, la.Renew(ctx), ErrLost)

	// release then re-acquire gives a higher fence
	require.NoError(t, lb.Release(ctx))
	la3, err := a.Acquire(ctx, "build", time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(3), la3.Fence)

	// independent names
	_, err = b.Acquire(ctx, "other", time.Minute)
	require.NoError(t, err)
}

func TestConcurrentClaimExactlyOneWinner(t *testing.T) {
	s := store.NewMemory()
	ids := []string{"m3", "m1", "m2"}
	for round := 0; round < 3; round++ {
		name := "reschedule:x" + string(rune('a'+round))
		var wg sync.WaitGroup
		res := make([]error, len(ids))
		for i, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m := &Manager{Store: s, Self: id, Settle: 150 * time.Millisecond, Now: time.Now}
				_, res[i] = m.Acquire(context.Background(), name, time.Minute)
			}()
		}
		wg.Wait()
		winners := 0
		for _, e := range res {
			if e == nil {
				winners++
			}
		}
		require.Equal(t, 1, winners)
	}
}

// Simultaneous claims (same fence written before anyone reads back) resolve to
// the lowest machine ID, and losers observe ErrHeld.
func TestSimultaneousClaimsLowestIDWins(t *testing.T) {
	s := store.NewMemory()
	ctx := context.Background()
	exp := time.Now().Add(time.Minute).UnixNano()
	for _, id := range []string{"m3", "m1", "m2"} {
		m := &Manager{Store: s, Self: id}
		require.NoError(t, m.writeClaim(ctx, Claim{Name: "x", Owner: id, Fence: 1, ExpiresUnixNano: exp}))
	}
	for _, id := range []string{"m3", "m2"} {
		m := &Manager{Store: s, Self: id, Now: time.Now}
		_, err := m.Acquire(ctx, "x", time.Minute)
		require.ErrorIs(t, err, ErrHeld)
	}
	h, err := (&Manager{Store: s, Now: time.Now}).Holder(ctx, "x")
	require.NoError(t, err)
	require.Equal(t, "m1", h.Owner)
	l, err := (&Manager{Store: s, Self: "m1", Now: time.Now}).Acquire(ctx, "x", time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), l.Fence)
}

func TestRunReleasesAndFailsOver(t *testing.T) {
	s := store.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := &Manager{Store: s, Self: "a", Settle: 0, Now: time.Now}
	started := make(chan int64, 1)
	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		m.Run(rctx, "ctl", 300*time.Millisecond, func(ctx context.Context, l *Lease) {
			started <- l.Fence
			<-ctx.Done()
		})
		close(done)
	}()
	require.Equal(t, int64(1), <-started)
	rcancel()
	<-done
	// released: another can take over immediately
	b := &Manager{Store: s, Self: "b", Now: time.Now}
	l, err := b.Acquire(ctx, "ctl", time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(2), l.Fence)
}
