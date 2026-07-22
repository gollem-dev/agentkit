package agentkit

import (
	"context"
	"log/slog"
	"sync"
)

// semaphore is a counting semaphore backed by a buffered channel. It bounds the
// number of claims driven concurrently in one Serve (poll loops + eager
// dispatch) to the hard limit (WithMaxConcurrent): a send takes a slot, a
// receive frees one. It counts *drivers*, not `running` rows — a driver that
// panics frees its slot while the row stays `running` until its lease expires.
type semaphore chan struct{}

func newSemaphore(n int) semaphore { return make(semaphore, n) }

// tryAcquire takes a slot without blocking and reports whether one was free.
func (s semaphore) tryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

// acquire blocks for a slot, returning false if ctx is done before one frees.
func (s semaphore) acquire(ctx context.Context) bool {
	select {
	case s <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s semaphore) release() { <-s }

// dispatcher is the in-process eager scheduler installed by Serve. When a
// Process becomes runnable on this instance (Spawn, Respond, a spawned child, a
// woken parent) it is submitted here and, if a hard-limit slot is free, driven
// immediately on a goroutine instead of waiting for the next poll. It is a pure
// latency optimization: everything it does, a poll-driven claim also does, and
// anything it drops (slot full, shutdown) is recovered by polling.
//
// The eager run uses d.ctx — the Serve context — for BOTH cancellation and
// values, exactly as a poll-driven claim and a crash-resume do. It deliberately
// does not inherit the caller's request context: making tool authorization or
// results depend on which trigger won the claim would change execution
// semantics, and a crash-resume could only ever see the Serve context anyway
// (ADR-0016). Request-scoped scope belongs in Process.Metadata (ADR-0011).
type dispatcher struct {
	k      *Kernel
	ctx    context.Context // Serve context: cancellation + value source for eager runs.
	cfg    serveConfig
	sem    semaphore
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// submit drives pid eagerly if a hard-limit slot is free; otherwise it is a
// no-op and a poller will pick the row up. Best-effort by construction.
//
// If runClaim exits by releasing the process back to pending (step budget
// spent), the slot is freed and the same pid is re-submitted so a long process
// keeps progressing under eager dispatch instead of falling back to polling.
func (d *dispatcher) submit(pid ProcessID) {
	d.mu.Lock()
	// closed is checked under the lock together with wg.Add so a submit can never
	// add to the WaitGroup after close() has passed its own closed check — that is
	// what makes close()'s Wait converge (it also stops re-submits below).
	if d.closed || d.ctx.Err() != nil {
		d.mu.Unlock()
		return
	}
	if !d.sem.tryAcquire() {
		d.mu.Unlock()
		return // hard limit full -> fall back to polling.
	}
	d.wg.Add(1)
	d.mu.Unlock()

	go func() {
		defer d.wg.Done()
		resubmit := false
		func() {
			// Release the slot before any re-submit so the freed slot is available to
			// the re-submit's tryAcquire, and recover a panic from outside the
			// transition boundary (claimSpecific, repo calls, runClaim's non-Step
			// parts) so a driver goroutine cannot take down the process. Step and
			// middleware panics are already recovered inside runTransition.
			defer d.sem.release()
			defer func() {
				if r := recover(); r != nil {
					d.k.logger.Error("eager dispatch panicked",
						slog.String("process", string(pid)), slog.Any("panic", r))
				}
			}()
			proc, ok := d.k.claimSpecific(d.ctx, pid, d.cfg)
			if !ok {
				return // another worker claimed it, or it is no longer pending.
			}
			resubmit = d.k.runClaim(d.ctx, d.cfg, proc) == claimReleased
		}()
		if resubmit {
			d.submit(pid) // slot already freed above; falls back to polling if full.
		}
	}()
}

// close stops accepting new work and drains in-flight eager runs. Setting closed
// under the lock guarantees no submit adds to wg afterwards, so Wait terminates.
func (d *dispatcher) close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.wg.Wait()
}

// dispatch submits a newly-runnable pid to the eager dispatcher, if one is
// installed (i.e. Serve is running on this instance). Without one it is a no-op
// and the row waits for a poll — the only case in which eager dispatch does not
// happen.
func (k *Kernel) dispatch(pid ProcessID) {
	if d := k.dispatcher.Load(); d != nil {
		d.submit(pid)
	}
}

// dispatchChildren eagerly dispatches every child buffered by this transition.
// Called after the commit that inserted them (so the pending rows are durable)
// and before the transition's OnCommit callbacks, so a slow handler cannot delay
// a runnable child (ADR-0016). Each dispatch is self-guarded by claimSpecific.
func (k *Kernel) dispatchChildren(sys *syscalls) {
	if sys == nil {
		return
	}
	for _, child := range sys.pendingChildren {
		k.dispatch(child.ID)
	}
}
