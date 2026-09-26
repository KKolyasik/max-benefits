// Package dispatch runs event handlers on a fixed pool of workers.
package dispatch

import (
	"context"
	"sync"
)

// Pool processes events concurrently across keys but strictly in order for
// the same key: events are sharded by key onto workers, each with its own
// queue. With users as keys, two quick button presses of one user can never
// race on the same session, while different users are served in parallel.
type Pool[T any] struct {
	queues []chan T
	key    func(T) int64
	handle func(T)
	wg     sync.WaitGroup
}

// New starts workers goroutines, each buffering up to buffer events.
func New[T any](workers, buffer int, key func(T) int64, handle func(T)) *Pool[T] {
	p := &Pool[T]{
		queues: make([]chan T, max(workers, 1)),
		key:    key,
		handle: handle,
	}
	for i := range p.queues {
		q := make(chan T, buffer)
		p.queues[i] = q
		p.wg.Go(func() {
			for ev := range q {
				p.handle(ev)
			}
		})
	}
	return p
}

// Submit enqueues an event, blocking while the worker's queue is full, which
// applies backpressure to the poller. It returns false if ctx is done first.
func (p *Pool[T]) Submit(ctx context.Context, ev T) bool {
	shard := uint64(p.key(ev)) % uint64(len(p.queues))
	select {
	case p.queues[shard] <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close stops accepting events and waits until the queued ones are handled.
// Submit must not be called after Close.
func (p *Pool[T]) Close() {
	for _, q := range p.queues {
		close(q)
	}
	p.wg.Wait()
}
