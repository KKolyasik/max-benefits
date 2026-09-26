package dispatch

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type event struct {
	user int64
	seq  int
}

func TestOrderPerKeyAndParallelismAcrossKeys(t *testing.T) {
	var (
		mu      sync.Mutex
		seen    = map[int64][]int{}
		running atomic.Int32
		peak    atomic.Int32
	)
	p := New(8, 4, func(e event) int64 { return e.user }, func(e event) {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		running.Add(-1)

		mu.Lock()
		seen[e.user] = append(seen[e.user], e.seq)
		mu.Unlock()
	})

	const users, perUser = 50, 20
	for seq := range perUser {
		for u := range users {
			p.Submit(context.Background(), event{user: int64(u), seq: seq})
		}
	}
	p.Close()

	for u := range int64(users) {
		if !slices.IsSorted(seen[u]) || len(seen[u]) != perUser {
			t.Fatalf("user %d: events out of order or lost: %v", u, seen[u])
		}
	}
	if peak.Load() < 2 {
		t.Fatalf("expected events of different users to run in parallel, peak %d", peak.Load())
	}
}

func TestSubmitStopsOnCancel(t *testing.T) {
	block := make(chan struct{})
	p := New(1, 0, func(e event) int64 { return 0 }, func(event) { <-block })
	ctx, cancel := context.WithCancel(context.Background())

	p.Submit(ctx, event{}) // taken by the worker, which now blocks
	cancel()
	if p.Submit(ctx, event{}) {
		t.Fatal("submit must give up once the context is cancelled")
	}
	close(block)
	p.Close()
}
