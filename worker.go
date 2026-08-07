package agentkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
)

// ServeOption configures Serve.
type ServeOption func(*serveConfig)

type serveConfig struct {
	workerID           string
	lease              time.Duration
	pollInterval       time.Duration
	maxStepsPerClaim   int
	maxStepAttempts    int
	maxUncleanReclaims int
	maxCancelDeferrals int
	pollConcurrency    int // soft limit: number of parallel poll (claim) loops.
	maxConcurrent      int // hard limit: max claims driven at once (poll + eager).
	retryBackoff       RetryBackoff
}

// RetryBackoff decides how long a requeued Process waits before it is claimable
// again. attempts is the error count the requeue is about to store, so the first
// failure of a transition asks for attempts=1.
//
// A fault that is not the strategy's — a ToolFactory error, a refused or failing
// Claim middleware — does not charge an attempt, so it asks with the count
// unchanged (0 unless an earlier transition already failed). A middleware that
// refuses every claim therefore keeps asking with the same number: return a
// constant there rather than expecting the curve to climb.
//
// It runs on the requeue path while the claim still holds its lease. Keep it
// pure and cheap — do not block, and do not reach for a store.
type RetryBackoff func(attempts int) time.Duration

// defaultRetryBackoff is the wait a requeued Process serves before it becomes
// claimable again: 2^attempts seconds, capped at a minute.
func defaultRetryBackoff(attempts int) time.Duration {
	// Clamping below keeps the shift count non-negative: a bogus stored
	// StepAttempts would otherwise panic at run time on a negative shift.
	n := min(max(0, attempts), 6)
	return min(time.Duration(1<<n)*time.Second, time.Minute)
}

// WithWorkerID sets the worker id (diagnostic). Default: hostname + "/" + uuid v7.
func WithWorkerID(id string) ServeOption { return func(c *serveConfig) { c.workerID = id } }

// WithLease sets the lease duration. Default: 60s.
func WithLease(d time.Duration) ServeOption { return func(c *serveConfig) { c.lease = d } }

// WithPollInterval sets the claim poll interval. Default: 500ms.
func WithPollInterval(d time.Duration) ServeOption {
	return func(c *serveConfig) { c.pollInterval = d }
}

// WithMaxStepsPerClaim sets how many transitions one claim runs. Default: 16.
// A value < 1 is treated as the default (0 would run no transition and release,
// which under eager dispatch re-submits in a tight loop).
func WithMaxStepsPerClaim(n int) ServeOption { return func(c *serveConfig) { c.maxStepsPerClaim = n } }

// WithMaxCancelDeferrals bounds how many transitions a claim may run after it
// observes a cancel request while the managed conversation still holds a tool
// call nobody answered. Default: one less than WithMaxStepsPerClaim, i.e. the
// rest of the claim. Zero finalizes at the first boundary, which is the
// behaviour from before this option existed. It is clamped below
// WithMaxStepsPerClaim so a cancel always lands inside the claim that observed
// it rather than surviving a release and starting the count over.
//
// Raising it does not make a cancel land on a usable transcript by itself: only
// a strategy that answers its tool calls produces a boundary worth waiting for.
// See docs/writing-strategies.md.
func WithMaxCancelDeferrals(n int) ServeOption {
	return func(c *serveConfig) { c.maxCancelDeferrals = n }
}

// WithMaxStepAttempts sets the step retry limit. Default: 3. This bounds
// attempts that ended in an ERROR; a claim that died mid-transition is bounded
// separately by WithMaxUncleanReclaims.
func WithMaxStepAttempts(n int) ServeOption { return func(c *serveConfig) { c.maxStepAttempts = n } }

// WithMaxUncleanReclaims bounds how many times a Process may be taken over
// after a claim died mid-transition. Default: 3. Exceeding it terminates the
// Process as failed with FailureUncleanReclaim.
//
// This is deliberately separate from WithMaxStepAttempts: an error tells the
// strategy how far the previous attempt got, whereas a vanished claim tells it
// nothing — the transition may have run every effect and died before its
// commit, and a lease-expiry reclaim may overlap a predecessor that is still
// running. Callers that cannot tolerate duplicated side effects set this to 0.
//
// The bound is compared as `UncleanReclaims > n`, matching the
// `StepAttempts+1 > n` convention: n permits n further attempts after the
// first. n=0 therefore finalizes the Process on the first unclean reclaim,
// without running Step at all.
func WithMaxUncleanReclaims(n int) ServeOption {
	return func(c *serveConfig) { c.maxUncleanReclaims = n }
}

// WithRetryBackoff sets the wait a requeued Process serves before it becomes
// claimable again. Default: 2^attempts seconds, capped at a minute. A nil
// function restores that default, and a negative duration is treated as zero.
//
// The kernel decides *whether* to retry (WithMaxStepAttempts) and you decide
// *how soon*. Two things the fixed default cannot express are the usual reasons
// to set it: jitter, so a fleet that failed together does not retry together;
// and a shorter curve in tests, which otherwise spend the real seconds.
//
//	agentkit.WithRetryBackoff(func(attempts int) time.Duration {
//		base := min(time.Duration(1<<min(attempts, 6))*time.Second, time.Minute)
//		return base + time.Duration(rand.Int64N(int64(base/4)))
//	})
//
// The wait is written to the Process as its wake time, and a pending Process is
// not claimable until it passes. A Repository that does not honour that (see the
// ClaimNextProcess contract) will retry as fast as it polls whatever this says.
func WithRetryBackoff(fn RetryBackoff) ServeOption {
	return func(c *serveConfig) { c.retryBackoff = fn }
}

// WithPollConcurrency sets the number of parallel poll (claim) loops — the soft
// limit on polling-driven concurrency. Default: 1. It is sub-capped by the hard
// limit (WithMaxConcurrent).
func WithPollConcurrency(n int) ServeOption {
	return func(c *serveConfig) { c.pollConcurrency = n }
}

// WithMaxConcurrent sets the hard limit: the maximum number of claims this Serve
// drives at once, counting both poll loops and eager dispatch. Default: 64. It
// is the capacity of a single semaphore shared by both; eager dispatch may burst
// up to it, and WithPollConcurrency is clamped to it. This bounds concurrent
// drivers, not `running` rows (a driver that panics frees its slot while the row
// stays running until its lease expires; other instances are not counted).
func WithMaxConcurrent(n int) ServeOption {
	return func(c *serveConfig) { c.maxConcurrent = n }
}

// defaultMaxConcurrent is the default hard limit. It is a moderate ceiling
// rather than a CPU-count derivation because a claim is I/O-bound (an LLM call),
// not CPU-bound; tune it to the deployment's LLM rate limits and memory.
const defaultMaxConcurrent = 64

// defaultMaxStepsPerClaim is the default (and clamp floor) for how many
// transitions one claim runs.
const defaultMaxStepsPerClaim = 16

// defaultMaxCancelDeferrals gives a cancelled Process the rest of its claim to
// close the conversation, so the claim itself is the bound rather than a second
// number chosen independently of it. A strategy that closes its tool rounds
// needs one transition; one that does not would not close in five either, so a
// smaller number saves nothing and only cuts short the legitimate shape that
// answers one call per transition.
const defaultMaxCancelDeferrals = defaultMaxStepsPerClaim - 1

func newServeConfig(opts []ServeOption) serveConfig {
	host, _ := os.Hostname()
	cfg := serveConfig{
		workerID:           host + "/" + uuid.Must(uuid.NewV7()).String(),
		lease:              60 * time.Second,
		pollInterval:       500 * time.Millisecond,
		maxStepsPerClaim:   defaultMaxStepsPerClaim,
		maxStepAttempts:    3,
		maxUncleanReclaims: 3,
		maxCancelDeferrals: defaultMaxCancelDeferrals,
		pollConcurrency:    1,
		maxConcurrent:      defaultMaxConcurrent,
		retryBackoff:       defaultRetryBackoff,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.retryBackoff == nil {
		cfg.retryBackoff = defaultRetryBackoff
	}
	// Clamp order matters: the hard limit must be >= 1 first (a zero hard limit
	// would let no poll loop ever acquire a slot, deadlocking Serve), then the
	// soft limit >= 1, then soft <= hard so poll loops can always acquire.
	if cfg.maxConcurrent < 1 {
		cfg.maxConcurrent = defaultMaxConcurrent
	}
	if cfg.pollConcurrency < 1 {
		cfg.pollConcurrency = 1
	}
	if cfg.pollConcurrency > cfg.maxConcurrent {
		cfg.pollConcurrency = cfg.maxConcurrent
	}
	// maxStepsPerClaim must be >= 1. Zero means "run no transition, release", and
	// under eager dispatch that release re-submits immediately — a tight claim ->
	// release -> re-dispatch loop that churns goroutines and hammers the store.
	if cfg.maxStepsPerClaim < 1 {
		cfg.maxStepsPerClaim = defaultMaxStepsPerClaim
	}
	// Clamped after maxStepsPerClaim, against its final value. A deferral bound at
	// or above it would let the claim end before the bound is reached: the Process
	// would be released, re-claimed, and start counting again, so a conversation
	// that never closes would keep a cancel pending indefinitely.
	if cfg.maxCancelDeferrals < 0 {
		cfg.maxCancelDeferrals = 0
	}
	if cfg.maxCancelDeferrals >= cfg.maxStepsPerClaim {
		cfg.maxCancelDeferrals = cfg.maxStepsPerClaim - 1
	}
	return cfg
}

// Serve runs claim loops until ctx is done (blocking). WithPollConcurrency loops
// share a workerID; the per-claim LeaseToken is the fence identity (D50). It also
// installs the eager dispatcher for this Kernel: a Process becoming runnable here
// (Spawn/Respond/child/parent) is driven immediately rather than at the next poll
// (ADR-0016). Only one Serve may be active per Kernel; a second returns
// ErrServeActive, because the dispatcher and its concurrency semaphore are
// per-Serve state that a second Serve would silently clobber.
func (k *Kernel) Serve(ctx context.Context, opts ...ServeOption) error {
	cfg := newServeConfig(opts)
	sem := newSemaphore(cfg.maxConcurrent)
	d := &dispatcher{k: k, ctx: ctx, cfg: cfg, sem: sem}
	if !k.dispatcher.CompareAndSwap(nil, d) {
		return goerr.Wrap(ErrServeActive, "another Serve is already active on this Kernel")
	}
	// Drain BEFORE clearing the pointer, not after. While close() stops new
	// submits and waits out in-flight eager runs, the pointer still points at the
	// (now closed) dispatcher, so dispatch() degrades to polling and a second
	// Serve still sees non-nil and gets ErrServeActive. Clearing first would let a
	// second Serve install its own dispatcher while this one's eager runs are
	// still draining — two live dispatchers, two semaphores, hard limit doubled.
	defer func() {
		d.close()
		k.dispatcher.CompareAndSwap(d, nil) // owner-checked: never clears a successor's pointer.
	}()
	var wg sync.WaitGroup
	for i := 0; i < cfg.pollConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k.serveLoop(ctx, cfg, sem)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// serveLoop polls for runnable Processes. It acquires a hard-limit slot before
// claiming (never after), so it never holds a claim while blocked for a slot —
// which would let the lease lapse and be charged as an unclean reclaim. The
// semaphore is shared with eager dispatch, bounding total concurrent claims to
// the hard limit.
func (k *Kernel) serveLoop(ctx context.Context, cfg serveConfig, sem semaphore) {
	for ctx.Err() == nil {
		if !sem.acquire(ctx) {
			return // ctx done.
		}
		proc, err := k.repo.ClaimNextProcess(ctx, cfg.workerID, k.clock().Add(cfg.lease), k.clock())
		if err != nil {
			sem.release()
			k.logger.Error("claim failed", "error", err)
			sleepOrDone(ctx, cfg.pollInterval)
			continue
		}
		if proc == nil {
			sem.release()
			sleepOrDone(ctx, cfg.pollInterval)
			continue
		}
		k.runClaim(ctx, cfg, proc)
		sem.release()
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// claimRun tracks what became of one claim while its chain runs. It is guarded
// because a middleware may hand `next` to another goroutine — which the contract
// forbids (see ClaimMiddleware), but a forbidden middleware must degrade to a
// clean abandon rather than to a data race and a write to a row someone is still
// driving.
type claimRun struct {
	mu        sync.Mutex
	entered   bool         // the base handler was called.
	completed bool         // driveClaim returned, so outcome is meaningful.
	outcome   ClaimOutcome // what driveClaim did to the row.
}

func (r *claimRun) enter() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entered {
		return false
	}
	r.entered = true
	return true
}

func (r *claimRun) finish(o ClaimOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed = true
	r.outcome = o
}

func (r *claimRun) read() (entered, completed bool, o ClaimOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entered, r.completed, r.outcome
}

// runClaim wraps one claim in the Claim middleware chain and settles whatever
// the chain leaves undone. The outcome it returns is what driveClaim actually
// did to the row, never what a middleware returned: eager dispatch re-submits on
// ClaimReleased, so a middleware reporting that for a claim which did not
// release would spin the same Process (the reason WithMaxStepsPerClaim(0) is
// clamped away, reached through the chain instead).
//
// Everything here runs under one recovery, chain construction included: a
// middleware that panics while being applied, or a ToolFactory that panics
// inside driveClaim, must not take the poll goroutine — and with it the
// process — down. serveLoop has no recovery of its own.
func (k *Kernel) runClaim(ctx context.Context, cfg serveConfig, proc *Process) (outcome ClaimOutcome) {
	claimToken := proc.LeaseToken // the fence identity for this claim (D50).
	run := &claimRun{}

	err := k.recoverClaim(func() error {
		base := func(c context.Context, _ *ClaimRequest) (ClaimOutcome, error) {
			if !run.enter() {
				return ClaimRefused, goerr.Wrap(ErrInvalidRequest,
					"claim middleware called next more than once")
			}
			o := k.driveClaim(c, cfg, proc, claimToken)
			run.finish(o)
			return o, nil
		}
		h := chainClaim(k.claimMW, base)
		if h == nil {
			return goerr.Wrap(ErrInvalidConfig, "claim middleware returned a nil handler")
		}
		// The Process handed to middleware is a copy for the same reason
		// StepRequest.Process is: the commit is built from the original. Paid for
		// only when a Claim middleware is actually registered.
		view := proc
		if len(k.claimMW) > 0 {
			view = proc.clone()
		}
		returned, cerr := h(ctx, &ClaimRequest{Process: view})
		if _, completed, real := run.read(); completed && cerr == nil && returned != real {
			k.logger.Warn("claim middleware rewrote the outcome; ignored",
				"process", proc.ID, "returned", string(returned), "actual", string(real))
		}
		return cerr
	})

	entered, completed, real := run.read()
	switch {
	case completed:
		// driveClaim settled the row, so a failure out here cannot change what was
		// committed — it is reported, not acted on.
		if err != nil {
			k.logger.Error("claim middleware failed after the claim ran",
				"process", proc.ID, "outcome", string(real), "error", err)
		}
		return real
	case entered && err == nil:
		// The base handler was called and has not returned, yet the chain has: a
		// middleware handed next to another goroutine. The claim still holds the
		// lease and will settle the row itself, so this frame must touch nothing.
		k.logger.Error("claim middleware returned before next completed; abandoning the frame",
			"process", proc.ID)
		return ClaimAbandoned
	case entered:
		// driveClaim did not finish: it panicked. Put the row back rather than
		// leaving it running until the lease lapses, which the next claim would
		// have to charge as an unclean reclaim (ADR-0015).
		return k.settleFailedClaim(ctx, cfg, proc, claimToken, goerr.Wrap(err, "claim panicked"))
	case err != nil:
		return k.settleFailedClaim(ctx, cfg, proc, claimToken, goerr.Wrap(err, "claim middleware"))
	default:
		// Refused: next was never called. Put the Process back with a backoff so a
		// middleware that keeps refusing throttles instead of spinning.
		return k.settleFailedClaim(ctx, cfg, proc, claimToken,
			goerr.New("claim refused by middleware", goerr.V("process", proc.ID)))
	}
}

// settleFailedClaim puts a Process back after the claim never ran or never
// finished. It reports ClaimAbandoned when the row could not be moved, because
// that — not "requeued" — is what happened to it.
func (k *Kernel) settleFailedClaim(ctx context.Context, cfg serveConfig, proc *Process,
	claimToken string, cause error) ClaimOutcome {
	if err := k.requeueInfra(ctx, cfg, proc, claimToken, cause); err != nil {
		return ClaimAbandoned
	}
	return ClaimRequeued
}

// recoverClaim runs fn and turns a panic into an error. runTransition already
// recovers a panic raised inside a transition, and callLimit the one at the
// boundary; this covers the rest of the claim scope — chain construction, the
// ToolFactory, a caller's RetryBackoff — none of which serveLoop guards.
func (k *Kernel) recoverClaim(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = goerr.New("claim panic", goerr.V("panic", fmt.Sprint(r)))
		}
	}()
	return fn()
}

// finalizeClaimed finalizes the Process and names what became of the row. Only a
// terminal commit that landed is ClaimFinished: a failure means the row is not
// terminal — the lease was lost, or the store refused — so this worker walked
// away from it and a later claim recovers it.
func (k *Kernel) finalizeClaimed(ctx context.Context, proc *Process, mut terminalMutator,
	claimToken string, foldMetrics Metrics) ClaimOutcome {
	if err := k.finalize(ctx, proc, mut, claimToken, foldMetrics); err != nil {
		k.logger.Error("finalize failed", "process", proc.ID, "error", err)
		return ClaimAbandoned
	}
	return ClaimFinished
}

// failOrRequeue finalizes the Process when its error budget is spent, and puts
// it back otherwise. The four places a transition can fail share it so they
// cannot drift apart.
func (k *Kernel) failOrRequeue(ctx context.Context, cfg serveConfig, proc *Process,
	claimToken string, cause error, foldMetrics Metrics) ClaimOutcome {
	if proc.StepAttempts+1 > cfg.maxStepAttempts {
		return k.finalizeClaimed(ctx, proc, failWith(FailureRetryExhausted, cause), claimToken, foldMetrics)
	}
	if err := k.requeueTransition(ctx, cfg, proc, claimToken, cause, foldMetrics); err != nil {
		return ClaimAbandoned
	}
	return ClaimRequeued
}

// driveClaim drives one claimed Process for up to maxStepsPerClaim transitions.
func (k *Kernel) driveClaim(ctx context.Context, cfg serveConfig, proc *Process, claimToken string) ClaimOutcome {
	b, err := k.agents.binding(proc.Agent)
	if err != nil {
		// Unknown agent: a permanent config mismatch (e.g. a binary generation skew).
		return k.finalizeClaimed(ctx, proc, failWith(FailureStrategyError, err), claimToken, Metrics{})
	}
	// One History holder per claim: the committed version is loaded once (on first
	// Session use) and advanced only when a transition commits, so it is shared
	// across this claim's transitions (ADR-0017). store is nil when the agent did
	// not opt in, in which case Session's methods return ErrHistoryNotConfigured.
	hs := &historyState{store: b.historyStore, pid: proc.ID}
	var toolList []gollem.Tool
	if k.toolFactory != nil {
		toolList, err = k.toolFactory(ctx, proc)
		if err != nil {
			return k.settleFailedClaim(ctx, cfg, proc, claimToken, goerr.Wrap(err, "tool factory"))
		}
	}
	k.expireDueAwaits(ctx, proc)

	// Claim-local, deliberately not persisted: the count only has to bound this
	// claim, because the clamp in newServeConfig keeps it below maxStepsPerClaim
	// and a cancel therefore always lands before the claim can end.
	cancelDeferrals := 0

	for i := 0; i < cfg.maxStepsPerClaim; i++ {
		fresh, err := k.repo.GetProcess(ctx, proc.ID)
		if err != nil {
			return ClaimAbandoned
		}
		if fresh.LeaseToken != claimToken {
			return ClaimAbandoned // lost the lease between transitions.
		}
		if fresh.CancelRequested {
			// Stopping here would leave the stored conversation ending on a tool call
			// nobody answered — unusable as a transcript to send anywhere. Running the
			// strategy's next transition is what closes it; the bound is what keeps a
			// conversation that never closes from holding a cancelled Process here.
			// The bound is tested first so a spent budget costs no History read.
			if cancelDeferrals >= cfg.maxCancelDeferrals || k.conversationClosed(ctx, hs, fresh) {
				return k.finalizeClaimed(ctx, fresh, cancelledWith(fresh.CancelReason), claimToken, Metrics{})
			}
			cancelDeferrals++
			k.logger.Info("cancel deferred: the managed conversation has an unanswered tool call",
				"process", fresh.ID, "deferrals", cancelDeferrals, "limit", cfg.maxCancelDeferrals)
		}
		proc = fresh
		// Bounded before Step rather than after a failure, because an unclean
		// reclaim is already counted by the time the Process is claimed: the
		// previous attempt may have run every effect and died before its commit.
		// Deciding here is what lets maxUncleanReclaims=0 mean "do not re-run at
		// all". In practice this only fires on the first iteration of a claim
		// that took over a dead one — a successful commit resets the counter, so
		// every later iteration reads 0.
		if proc.UncleanReclaims > cfg.maxUncleanReclaims {
			cause := goerr.New("unclean reclaim limit exceeded",
				goerr.V("unclean_reclaims", proc.UncleanReclaims),
				goerr.V("limit", cfg.maxUncleanReclaims))
			return k.finalizeClaimed(ctx, proc, failWith(FailureUncleanReclaim, cause), claimToken, Metrics{})
		}
		// The verdict is kept, not just tested: a notice seeds Syscalls.LimitStatus()
		// for this transition, which is how a strategy learns the budget is running
		// out before anything refuses it.
		//
		// Limit is the strategy's own method and this call is outside the recover in
		// runTransition, so a panic goes through failOrRequeue like any other failed
		// transition. recoverClaim would catch it too, but as a claim panic —
		// requeued as infrastructure, which does not spend StepAttempts, so a Limit
		// that always panics would requeue forever. Charging it to the strategy
		// bounds it at retry_exhausted. No effect has run yet, hence Metrics{}.
		limit, lerr := callLimit(ctx, b.limit, proc, proc.Metrics)
		if lerr != nil {
			return k.failOrRequeue(ctx, cfg, proc, claimToken, lerr, Metrics{})
		}
		if limit.Kind() == LimitKindStop {
			return k.finalizeClaimed(ctx, proc,
				failWithMessage(FailureLimitExceeded, limit.Message()), claimToken, Metrics{})
		}

		sys := newSyscalls(k, proc, toolList, hs, b.limit, limit)
		// The History version this transition starts from. Once its own version is
		// committed, this one is no longer referenced and the store is told.
		prevRef := proc.HistoryRef
		rawState, dec, terr := k.runTransition(ctx, sys, b, proc)
		if terr != nil {
			// This transition did not commit; its buffered children are dropped.
			sys.notifySpawnDone(terr)
			return k.failOrRequeue(ctx, cfg, proc, claimToken, terr, sys.runMetrics)
		}

		if dec.kind == DecisionDone || dec.kind == DecisionFail {
			// Persist History before the terminal commit too, so a later
			// restart/handoff can read the final transcript (ADR-0017, D-D). A save
			// failure is treated like a transition failure: do not commit, requeue.
			// No lease fence is needed: a stored version is never rewritten, so a
			// worker that lost its lease can only add one nobody references.
			if serr := sys.saveHistory(ctx); serr != nil {
				sys.notifySpawnDone(serr)
				return k.failOrRequeue(ctx, cfg, proc, claimToken, serr, sys.runMetrics)
			}
			// commitTerminal fires the spawn OnCommit callbacks and eager dispatch
			// itself (nil on commit, err on abandon). A failure here means the row is
			// NOT terminal, so it must not be reported as finished — and it leaves
			// the commit outcome unknown, which is why nothing is discarded on it.
			committed, cterr := k.commitTerminal(ctx, proc, rawState, b.version, sys.seq, dec, sys, claimToken)
			if cterr != nil {
				return ClaimAbandoned
			}
			// Only this call's own Apply supersedes prevRef. When another path
			// finished the Process first, the record still names prevRef and
			// releasing it would destroy the transcript that path committed.
			if committed {
				discardSuperseded(ctx, b.historyStore, proc.ID, prevRef, sys.sessRef)
			}
			return ClaimFinished
		}

		// Save before building the commit: buildCommit records the ref this returns
		// (ADR-0017 — commit is the completion marker, so durable work precedes it).
		// It does NOT advance the committed baseline; commitHistory does that only
		// after Apply succeeds, so a conflict retry re-seeds from committed state.
		if serr := sys.saveHistory(ctx); serr != nil {
			sys.notifySpawnDone(serr)
			return k.failOrRequeue(ctx, cfg, proc, claimToken, serr, sys.runMetrics)
		}
		cs, cerr := k.buildCommit(ctx, proc, rawState, b.version, sys.seq, dec, sys, cfg)
		if cerr != nil {
			// Suspend-without-await, invalid child ref, etc. -> retry path. Nothing
			// committed, so this attempt's version is unreferenced for good.
			discardUncommitted(ctx, b.historyStore, proc.ID, prevRef, sys.sessRef)
			sys.notifySpawnDone(cerr)
			return k.failOrRequeue(ctx, cfg, proc, claimToken, cerr, sys.runMetrics)
		}
		if err := k.repo.Apply(ctx, cs.changeSet); err != nil {
			if errors.Is(err, ErrConflict) {
				// A conflict is the one failure that proves nothing committed, so this
				// attempt's version can be released. Any other error leaves the outcome
				// unknown and the version has to stay: discarding it could destroy the
				// one the record now names.
				discardUncommitted(ctx, b.historyStore, proc.ID, prevRef, sys.sessRef)
				sys.notifySpawnDone(err) // this attempt's buffered children did not commit.
				cur, gerr := k.repo.GetProcess(ctx, proc.ID)
				if gerr != nil || cur == nil || cur.LeaseToken != claimToken {
					return ClaimAbandoned // lost the lease -> abandon (never rebase, D50).
				}
				proc = cur
				i--
				continue // same-lease race (Cancel etc.) -> rebuild.
			}
			sys.notifySpawnDone(err)
			return ClaimAbandoned
		}
		// Committed: eager-dispatch buffered children before firing OnCommit, so a
		// slow handler cannot delay a runnable child (ADR-0016). Then the callbacks.
		k.dispatchChildren(sys)
		sys.notifySpawnDone(nil)
		sys.commitHistory() // advance the committed History baseline (ADR-0017).
		discardSuperseded(ctx, b.historyStore, proc.ID, prevRef, sys.sessRef)
		switch {
		case cs.suspend:
			return ClaimSuspended // waiting committed.
		case cs.elidedRunning:
			// WaitChildren elision: children already done; continue this claim.
			continue
		default:
			continue // Continue: next transition (loop re-reads fresh).
		}
	}
	if err := k.release(ctx, proc, claimToken); err != nil {
		return ClaimAbandoned
	}
	return ClaimReleased
}

// claimSpecific claims one pending Process by id, for eager dispatch. It writes
// running + a fresh LeaseToken via an ordinary Rev-CAS Apply — the same fence a
// poll claim uses — so it races ClaimNextProcess safely: whoever advances the
// Rev first wins, the loser sees ErrConflict and abandons. It targets pending
// ONLY; a running row (an expired lease) is left to ClaimNextProcess, which is
// the sole path that counts unclean reclaims, so eager never inflates that
// counter.
//
// A normal loss — ErrConflict, a not-found row, or a status that is no longer
// pending — is silent (a poller or another dispatch got there first). Any other
// error is a repository fault worth surfacing, so it is logged before abandoning
// (the row is recovered by polling either way).
func (k *Kernel) claimSpecific(ctx context.Context, pid ProcessID, cfg serveConfig) (*Process, bool) {
	proc, err := k.repo.GetProcess(ctx, pid)
	if err != nil {
		if !errors.Is(err, ErrProcessNotFound) {
			k.logger.Error("eager claim: get failed", "process", pid, "error", err)
		}
		return nil, false
	}
	if proc.Status != ProcessPending {
		return nil, false
	}
	now := k.clock()
	if proc.WakeAt != nil && proc.WakeAt.After(now) {
		// Still inside the retry backoff. Eager dispatch has to honour the same
		// claim predicate as ClaimNextProcess, or it would hand a failing Process
		// straight back to a worker and the backoff would only apply to polling.
		return nil, false
	}
	c := proc.clone()
	c.Status = ProcessRunning
	c.LeaseOwner = cfg.workerID
	c.LeaseToken = uuid.Must(uuid.NewV7()).String() // fresh fence identity per claim.
	lu := now.Add(cfg.lease)
	c.LeaseUntil = &lu
	c.UpdatedAt = now
	// Rev stays proc.Rev so Apply's CAS advances it by one; a conflict means
	// another worker claimed the row first.
	if err := k.repo.Apply(ctx, ChangeSet{Processes: []*Process{c}}); err != nil {
		if !errors.Is(err, ErrConflict) {
			// A non-conflict Apply error may have committed indeterminately (e.g. the
			// filesystem store's post-rename failure): the row could already be
			// running with our token. We still abandon; the lease then expires and a
			// poller reclaims it (counted as an unclean reclaim). Rare, bounded by
			// WithMaxUncleanReclaims, and always recovered by polling.
			k.logger.Error("eager claim: apply failed", "process", pid, "error", err)
		}
		return nil, false
	}
	c.Rev = proc.Rev + 1
	return c, true
}

// runTransition decodes state, runs Step, and encodes the result. Panics are
// recovered as errors (E6).
func (k *Kernel) runTransition(ctx context.Context, sys *syscalls, b StrategyBinding, proc *Process) (raw []byte, dec decision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = goerr.New("strategy panic", goerr.V("panic", fmt.Sprint(r)))
		}
	}()
	st, err := b.decode(proc.StateVersion, proc.State)
	if err != nil {
		return nil, decision{}, goerr.Wrap(err, "decode state")
	}
	// The Step middleware chain sits between decode and encode. It wraps the
	// Step CALL only — the commit happens after this function returns, so a
	// middleware never learns from here whether the transition was persisted.
	//
	// Unlike an effect handler, this one may run at most once per transition.
	// Step's side effects (spawned children, emitted events, metrics) accumulate
	// in `sys`, which is per-transition and not per-call, so a second attempt
	// would commit the first attempt's buffered effects alongside its own state
	// and Decision. Rather than let that happen silently, the second call is an
	// error.
	stepped := false
	base := func(c context.Context, r *StepRequest) (*StepResult, error) {
		if stepped {
			return nil, goerr.Wrap(ErrInvalidRequest,
				"step middleware called next more than once (effects buffer per transition, not per call)")
		}
		stepped = true
		ns, d, serr := b.step(c, r.Sys, r.state)
		if serr != nil {
			return nil, serr
		}
		return &StepResult{dec: d, state: ns}, nil
	}
	h := chainStep(k.stepMW, base)
	if h == nil {
		return nil, decision{}, goerr.Wrap(ErrInvalidConfig, "step middleware returned a nil handler")
	}
	// The Process handed to middleware is a copy: `proc` is the row the commit is
	// built from, so a middleware writing to it (Metadata, or Rev, which would
	// make every Apply conflict) would corrupt durable state and fencing. The
	// clone is only paid for when a Step middleware is actually registered.
	view := proc
	if len(k.stepMW) > 0 {
		view = proc.clone()
	}
	res, err := h(ctx, &StepRequest{Effect: sys.ec(), Process: view, Sys: sys, state: st})
	if err != nil {
		return nil, decision{}, err
	}
	if res == nil {
		return nil, decision{}, goerr.Wrap(ErrInvalidConfig, "step middleware returned a nil result")
	}
	dec = res.dec
	raw, err = b.encode(res.state)
	if err != nil {
		return nil, decision{}, goerr.Wrap(err, "encode state")
	}
	// EncodeOutput runs here rather than inside b.step because a middleware may
	// have replaced the Decision on the way out, and it has no way to encode.
	if dec.hasOut {
		out, oerr := b.encodeOutput(dec.typed)
		if oerr != nil {
			return nil, decision{}, goerr.Wrap(oerr, "encode output")
		}
		dec.output = out
	}
	// A nil output is the one thing the kernel can meaningfully check about
	// caller data (ADR-0007).
	if dec.kind == DecisionDone && dec.output == nil {
		return nil, decision{}, goerr.New("Done with nil output")
	}
	return raw, dec, nil
}

// terminalMutator mutates a Process into a terminal state.
type terminalMutator func(*Process)

func cancelledWith(reason string) terminalMutator {
	return func(p *Process) { p.Status = ProcessCancelled; p.CancelReason = reason }
}

func failWith(code FailureCode, err error) terminalMutator {
	return failWithMessage(code, err.Error())
}

// failWithMessage is the form for a reason that was never an error to begin
// with — a Limit verdict's, which is a string by design (the error type would be
// discarded here anyway).
func failWithMessage(code FailureCode, msg string) terminalMutator {
	return func(p *Process) { p.Status = ProcessFailed; p.Failure = &Failure{Code: code, Message: msg} }
}

// commitResult carries a built non-terminal commit plus how the worker should proceed.
type commitResult struct {
	changeSet     ChangeSet
	suspend       bool // committed as waiting.
	elidedRunning bool // WaitChildren fully elided -> keep running this claim.
}

// buildCommit builds the Continue/Suspend commit ChangeSet. WaitChildren specs
// are resolved against "Repository snapshot + pending buffered children" and
// elided when all children are already terminal (D46/#4).
func (k *Kernel) buildCommit(ctx context.Context, proc *Process, rawState []byte, version, seq int, dec decision, sys *syscalls, cfg serveConfig) (commitResult, error) {
	now := k.clock()
	p := proc.clone()
	p.State = rawState
	p.StateVersion = version
	p.StateSeq = seq
	p.HistoryRef = sys.nextHistoryRef()
	p.StepAttempts = 0
	p.UncleanReclaims = 0
	p.Metrics = p.Metrics.add(sys.runMetrics).add(Metrics{Steps: 1})
	p.UpdatedAt = now

	cs := ChangeSet{}
	cs.Processes = append(cs.Processes, p)
	cs.Processes = append(cs.Processes, sys.pendingChildren...)
	cs.Events = append(cs.Events, sys.pendingEvents...)

	if dec.kind == DecisionContinue {
		p.Status = ProcessRunning
		p.WakeAt = nil
		lease := now.Add(cfg.lease)
		p.LeaseUntil = &lease
		return commitResult{changeSet: cs}, nil
	}

	// Suspend: build declared awaits.
	var awaitRows []*Await
	var wakeCandidates []time.Time
	openAwaits := 0
	elided := 0
	for _, spec := range dec.awaits {
		if ex, ok := sys.awaits[spec.key]; ok && ex.Status != AwaitOpen {
			continue // responded/expired/cancelled key -> no-op re-declaration.
		}
		_, existed := sys.awaits[spec.key]
		switch spec.kind {
		case AwaitQuestion:
			awaitRows = append(awaitRows, &Await{
				ProcessID: proc.ID, Key: spec.key, Kind: AwaitQuestion, Status: AwaitOpen,
				Question: spec.payload, Deadline: spec.deadline, CreatedAt: now,
			})
			if !existed {
				cs.Events = append(cs.Events, newEvent(proc.ID, EventAwaitCreated, spec.key, nil, now))
			}
			openAwaits++
			if spec.deadline != nil {
				wakeCandidates = append(wakeCandidates, *spec.deadline)
			}
		case AwaitTimer:
			awaitRows = append(awaitRows, &Await{
				ProcessID: proc.ID, Key: spec.key, Kind: AwaitTimer, Status: AwaitOpen,
				Deadline: spec.deadline, CreatedAt: now,
			})
			openAwaits++
			if spec.deadline != nil {
				wakeCandidates = append(wakeCandidates, *spec.deadline)
			}
		case AwaitChildren:
			row, guards, allTerminal, err := k.resolveWaitChildren(ctx, proc.ID, spec, sys, now)
			if err != nil {
				return commitResult{}, err
			}
			awaitRows = append(awaitRows, row)
			cs.Guards = append(cs.Guards, guards...)
			if allTerminal {
				// No fold here: each of these children already reported its usage to
				// this Process when it terminated (reportToParent).
				elided++
			} else {
				openAwaits++
			}
		}
	}
	cs.Awaits = append(cs.Awaits, awaitRows...)

	// Any pre-existing open await also keeps the Process waiting (allows a
	// specs-less Suspend()).
	preOpen := false
	for _, aw := range sys.awaits {
		if aw.Status == AwaitOpen {
			preOpen = true
			if aw.Deadline != nil {
				wakeCandidates = append(wakeCandidates, *aw.Deadline)
			}
		}
	}

	switch {
	case openAwaits > 0 || preOpen:
		p.Status = ProcessWaiting
		p.WakeAt = minTime(wakeCandidates)
		p.LeaseUntil = nil
		return commitResult{changeSet: cs, suspend: true}, nil
	case elided > 0:
		// All waited children already terminal: continue this claim.
		p.Status = ProcessRunning
		p.WakeAt = nil
		lease := now.Add(cfg.lease)
		p.LeaseUntil = &lease
		return commitResult{changeSet: cs, elidedRunning: true}, nil
	default:
		return commitResult{}, goerr.Wrap(ErrSuspendWithoutAwait, "suspend produced no open await")
	}
}

// resolveWaitChildren resolves a WaitChildren spec against the Repository plus
// this transition's pending buffered children. Pending children count as
// pending (not yet persisted, so no Guard); existing children contribute their
// Rev to Guards so a concurrent finalize is detected (#3a/#4).
func (k *Kernel) resolveWaitChildren(ctx context.Context, pid ProcessID, spec AwaitSpec, sys *syscalls, now time.Time) (*Await, []ProcessGuard, bool, error) {
	allTerminal := true
	var guards []ProcessGuard
	var results []ChildResult
	for _, cid := range spec.children {
		if pendingChild(cid, sys.pendingChildren) {
			allTerminal = false
			continue
		}
		cp, err := k.repo.GetProcess(ctx, cid)
		if err != nil {
			return nil, nil, false, goerr.Wrap(ErrInvalidRequest, "WaitChildren references unknown child",
				goerr.V("child", cid))
		}
		// A WaitChildren id must be a direct child of this Process (a pending
		// buffered child handled above, or an existing child with ParentID==pid).
		// Otherwise a strategy could wait on and read any Process by id (#4).
		if cp.ParentID == nil || *cp.ParentID != pid {
			return nil, nil, false, goerr.Wrap(ErrInvalidRequest, "WaitChildren references a non-child process",
				goerr.V("child", cid), goerr.V("parent", pid))
		}
		guards = append(guards, ProcessGuard{ProcessID: cid, Rev: cp.Rev})
		if cp.Status.Terminal() {
			results = append(results, childResultOf(cp))
		} else {
			allTerminal = false
		}
	}
	row := &Await{ProcessID: pid, Key: spec.key, Kind: AwaitChildren, Children: spec.children, CreatedAt: now}
	if allTerminal {
		row.Status = AwaitResponded
		row.Results = results
		row.RespondedAt = &now
	} else {
		row.Status = AwaitOpen
	}
	return row, guards, allTerminal, nil
}

func pendingChild(cid ProcessID, pending []*Process) bool {
	for _, c := range pending {
		if c.ID == cid {
			return true
		}
	}
	return false
}

func childResultOf(cp *Process) ChildResult {
	return ChildResult{
		ProcessID: cp.ID,
		Status:    cp.Status,
		Output:    cp.Output,
		Failure:   cp.Failure,
		Metrics:   cp.Metrics,
	}
}

func minTime(ts []time.Time) *time.Time {
	if len(ts) == 0 {
		return nil
	}
	m := ts[0]
	for _, t := range ts[1:] {
		if t.Before(m) {
			m = t
		}
	}
	return &m
}

// commitTerminal commits a Done/Fail transition and its finalize in a single
// Apply (#3/D47). It carries the transition state and folds this run's metrics.
// The bool reports whether THIS call's Apply is what committed; see commitFinal.
func (k *Kernel) commitTerminal(ctx context.Context, proc *Process, rawState []byte, version, seq int, dec decision, sys *syscalls, fenceToken string) (bool, error) {
	return k.commitFinal(ctx, proc, fenceToken, func(p *Process) {
		p.State = rawState
		p.StateVersion = version
		p.StateSeq = seq
		p.HistoryRef = sys.nextHistoryRef()
		p.StepAttempts = 0
		p.UncleanReclaims = 0
		p.Metrics = p.Metrics.add(sys.runMetrics).add(Metrics{Steps: 1})
		if dec.kind == DecisionDone {
			p.Status = ProcessSucceeded
			p.Output = dec.output
		} else {
			p.Status = ProcessFailed
			p.Failure = dec.failure
		}
	}, sys, dec.typed)
}

// finalize commits an external termination (unknown agent / cancel / limit /
// retry_exhausted). State is left as-is; foldMetrics is added to Process.Metrics
// (retry_exhausted folds the run's consumed metrics, #5).
func (k *Kernel) finalize(ctx context.Context, proc *Process, mut terminalMutator, fenceToken string, foldMetrics Metrics) error {
	// Which Apply won does not matter here: nothing downstream of an external
	// termination depends on this call being the committing one.
	_, err := k.commitFinal(ctx, proc, fenceToken, func(p *Process) {
		mut(p)
		p.Metrics = p.Metrics.add(foldMetrics)
	}, nil, nil)
	return err
}

// commitFinal commits a terminal Process plus its finalize (open awaits
// cancelled, process.finished, parent wakeup) in a single Apply, retrying on
// conflict by re-reading (and re-evaluating the parent). If the lease was lost
// (LeaseToken changed) or the Process is already terminal, it stops. sys may be
// nil for external terminations (no buffered children / spawn OnCommit callbacks).
// The fenceToken argument distinguishes two callers:
//   - a worker's terminal commit passes its claim's LeaseToken (non-empty): a
//     conflict whose stored token no longer matches means the lease was lost, so
//     it abandons (never rebases Rev, #2/D50).
//   - an external caller (Cancel) passes "" (externalFence): a conflict is
//     propagated as ErrConflict so the caller re-reads and re-decides — it must
//     NOT be silently abandoned (#1).
//
// typedOut is the value Done received, forwarded to the completion handler
// after the commit (nil for every non-Done termination).
//
// A required read failure while building the finalize ChangeSet (own awaits,
// parent, siblings) aborts the whole commit and returns the error, leaving the
// Process non-terminal for lease-expiry retry (#2) — never a partial finalize.
//
// The bool reports whether THIS call's Apply is what made the Process terminal.
// It is false, with a nil error, in the one case where the Process ended up
// terminal without this call writing it: a conflict whose re-read finds it
// already terminal by another path. A caller that acts on its own committed
// values — releasing the History version its commit superseded, say — must check
// it, because in that case the record still holds whatever the winning path
// wrote, not what this mutate produced.
const externalFence = ""

func (k *Kernel) commitFinal(ctx context.Context, proc *Process, fenceToken string, mutate func(*Process), sys *syscalls, typedOut any) (bool, error) {
	for {
		now := k.clock()
		p := proc.clone()
		mutate(p)
		p.UpdatedAt = now
		p.WakeAt = nil
		p.LeaseUntil = nil

		cs := ChangeSet{Processes: []*Process{p}}
		cs.Events = append(cs.Events, newEvent(p.ID, EventProcessFinished, "", nil, now))
		if sys != nil {
			cs.Processes = append(cs.Processes, sys.pendingChildren...)
			cs.Events = append(cs.Events, sys.pendingEvents...)
		}
		// Close this Process's open awaits.
		awaits, err := k.repo.ListAwaits(ctx, p.ID)
		if err != nil {
			return false, k.abortFinal(sys, goerr.Wrap(err, "list own awaits for finalize"))
		}
		for _, aw := range awaits {
			if aw.Status == AwaitOpen {
				aw.Status = AwaitCancelled
				cs.Awaits = append(cs.Awaits, aw)
			}
		}
		// Fold this Process's usage into its parent, and wake the parent if this
		// completes an open children await.
		if p.ParentID != nil {
			if err := k.reportToParent(ctx, *p.ParentID, p, &cs, now); err != nil {
				return false, k.abortFinal(sys, err)
			}
		}

		err = k.repo.Apply(ctx, cs)
		if errors.Is(err, ErrConflict) {
			fresh, gerr := k.repo.GetProcess(ctx, proc.ID)
			if gerr != nil || fresh == nil || fresh.Status.Terminal() {
				// Already terminal by another path: the Process is finished, but this
				// Apply is not what finished it, so the row holds the winner's values
				// and not the ones mutate produced. Reported as (false, nil).
				return false, k.abortFinal(sys, nil)
			}
			if fenceToken == externalFence {
				return false, goerr.Wrap(ErrConflict, "external finalize lost a race; retry") // caller re-reads (#1).
			}
			if fresh.LeaseToken != fenceToken {
				// The worker lost the lease: abandon without rebasing Rev (#2/D50).
				// This is reported rather than swallowed, because the row is NOT
				// terminal — the caller would otherwise announce ClaimFinished for a
				// Process another worker now owns.
				return false, k.abortFinal(sys, goerr.Wrap(ErrConflict,
					"lost the lease before the terminal commit", goerr.V("process", proc.ID)))
			}
			proc = fresh
			continue
		}
		if err != nil {
			return false, k.abortFinal(sys, err)
		}
		// Eager-dispatch after the durable commit, before user callbacks (ADR-0016):
		// any child buffered by this (terminal) transition, and the parent this
		// termination may have woken to pending. Both are self-guarded by
		// claimSpecific, so dispatching a not-actually-woken parent is a no-op.
		k.dispatchChildren(sys)
		if p.ParentID != nil {
			k.dispatch(*p.ParentID)
		}
		if sys != nil {
			sys.notifySpawnDone(nil)
		}
		k.fireFinish(ctx, p, typedOut)
		return true, nil
	}
}

// fireFinish invokes the agent's completion handler after a terminal commit has
// been persisted. Best-effort by construction: it cannot fire twice (a losing
// racer abandons before reaching here), but a crash between the Apply and this
// call loses the notification and there is no retry (ADR-0014). Neither an
// error nor a panic from the handler changes the committed Process.
func (k *Kernel) fireFinish(ctx context.Context, p *Process, typedOut any) {
	b, err := k.agents.binding(p.Agent)
	if err != nil || b.finish == nil {
		return
	}
	// The worker's ctx is cancelled on shutdown, which would otherwise drop the
	// notification for every Process finishing during a drain. The handler owns
	// its own deadline.
	hctx := context.WithoutCancel(ctx)
	defer func() {
		if r := recover(); r != nil {
			k.logger.Error("finish handler panicked",
				"process", p.ID, "agent", p.Agent, "panic", fmt.Sprint(r))
		}
	}()
	if ferr := b.finish(hctx, p.ID, p.Status, typedOut, p.Failure); ferr != nil {
		k.logger.Error("finish handler failed",
			"process", p.ID, "agent", p.Agent, "error", ferr)
	}
}

// abortFinal notifies buffered spawn OnCommit callbacks that the commit did not
// happen and returns the (possibly nil) error unchanged.
func (k *Kernel) abortFinal(sys *syscalls, err error) error {
	if sys != nil {
		cause := err
		if cause == nil {
			cause = goerr.New("transition not committed (process finalized by another path)")
		}
		sys.notifySpawnDone(cause)
	}
	return err
}

// reportToParent, on a child finalize, does two things in the child's own
// terminal ChangeSet: it folds the child's usage into the parent, and it
// responds+wakes any open children await this child completes (with this
// child's terminal state overlaid).
//
// The fold is here, at the child's single terminal transition, rather than where
// an await resolves. Keying it to the await made it run once per await instead
// of once per child, so a child named by two await keys was counted twice and a
// child nobody waited on — which ADR-0009 permits — was never counted at all.
// A Process terminates once, so counting there is right by construction rather
// than by a dedup check.
//
// The parent row is a CAS target whenever a parent exists, which also serializes
// sibling finalizes on the parent Rev (closing the #3c write skew). Any required
// read failure returns an error so the caller aborts the finalize (never a
// partial commit that would lose the wakeup, #2). A parent that no longer exists
// is treated as "nothing to report to".
func (k *Kernel) reportToParent(ctx context.Context, parentID ProcessID, child *Process, cs *ChangeSet, now time.Time) error {
	parent, err := k.repo.GetProcess(ctx, parentID)
	if err != nil {
		if errors.Is(err, ErrProcessNotFound) {
			return nil // no parent to wake.
		}
		return goerr.Wrap(err, "get parent for wakeup", goerr.V("parent", parentID))
	}
	awaits, err := k.repo.ListAwaits(ctx, parent.ID)
	if err != nil {
		return goerr.Wrap(err, "list parent awaits for wakeup", goerr.V("parent", parentID))
	}
	pClone := parent.clone()
	// One Process, one terminal transition, one fold.
	pClone.Metrics = pClone.Metrics.add(child.Metrics)
	for _, aw := range awaits {
		if aw.Kind != AwaitChildren || aw.Status != AwaitOpen || !containsID(aw.Children, child.ID) {
			continue
		}
		allTerminal := true
		var results []ChildResult
		for _, cid := range aw.Children {
			if cid == child.ID {
				results = append(results, childResultOf(child))
				continue
			}
			sib, gerr := k.repo.GetProcess(ctx, cid)
			if gerr != nil {
				// A sibling in an open children await must be readable; a transient
				// failure must not let us commit the last child terminal and lose the
				// wakeup. Abort and retry (#2).
				return goerr.Wrap(gerr, "get sibling for wakeup", goerr.V("sibling", cid))
			}
			if !sib.Status.Terminal() {
				allTerminal = false
				continue
			}
			results = append(results, childResultOf(sib))
		}
		if allTerminal {
			aw.Status = AwaitResponded
			aw.Results = results
			aw.RespondedAt = &now
			if pClone.Status == ProcessWaiting {
				pClone.Status = ProcessPending
				pClone.WakeAt = nil // same reason as Respond: WakeAt gates a pending claim.
			}
			cs.Awaits = append(cs.Awaits, aw)
		}
	}
	// The parent row is always a CAS target: it carries the fold, and CASing it
	// unconditionally is what serializes sibling finalizes on the parent Rev
	// (#3c). A parent that spawned this child and never waited is written too —
	// that is the case the fold exists for.
	cs.Processes = append(cs.Processes, pClone)
	return nil
}

func containsID(ids []ProcessID, id ProcessID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// requeueTransition puts the Process back after a transition that failed,
// consuming one step attempt. A non-nil error means the row was NOT put back.
func (k *Kernel) requeueTransition(ctx context.Context, cfg serveConfig, proc *Process, fenceToken string, cause error, foldMetrics Metrics) error {
	return k.requeue(ctx, cfg, proc, fenceToken, cause, foldMetrics, true)
}

// requeueInfra puts the Process back after a fault that is not the strategy's
// (a ToolFactory failure, say), leaving the attempt counter alone. Charging an
// attempt here would also make the next transition look like a replay through
// Syscalls.Attempt(), which it is not: Step never ran.
func (k *Kernel) requeueInfra(ctx context.Context, cfg serveConfig, proc *Process, fenceToken string, cause error) error {
	return k.requeue(ctx, cfg, proc, fenceToken, cause, Metrics{}, false)
}

// requeue puts the Process back to pending with a backoff, folding this run's
// metrics on the successful Apply (#5).
//
// A conflict here must not be shrugged off. Leaving the row `running` would let
// the lease lapse and the next claim would count the takeover as an unclean
// reclaim (ADR-0015) — turning an error the worker actually observed into a
// phantom crash, and charging the wrong budget. So a conflict is re-read and
// rebuilt against fresh state, exactly like a terminal commit, and abandoned
// only when the lease is genuinely gone.
//
// It returns nil only when the row really is back at pending. The caller reports
// that difference as ClaimRequeued versus ClaimAbandoned, so a store that
// refused the write does not surface as a successful requeue.
func (k *Kernel) requeue(ctx context.Context, cfg serveConfig, proc *Process, fenceToken string, cause error, foldMetrics Metrics, consumeAttempt bool) error {
	for {
		now := k.clock()
		p := proc.clone()
		p.Status = ProcessPending
		if consumeAttempt {
			p.StepAttempts = proc.StepAttempts + 1
		}
		// The wake time is what holds the Process back: a pending row is not a
		// claim target until it passes (see the Repository contract). A caller's
		// curve is not trusted to be non-negative, which would put the wake time in
		// the past and make the backoff a no-op.
		backoff := max(0, cfg.retryBackoff(p.StepAttempts))
		wake := now.Add(backoff)
		p.WakeAt = &wake
		p.LeaseUntil = nil
		p.UpdatedAt = now
		p.Metrics = p.Metrics.add(foldMetrics)
		err := k.repo.Apply(ctx, ChangeSet{Processes: []*Process{p}})
		if errors.Is(err, ErrConflict) {
			fresh, gerr := k.repo.GetProcess(ctx, proc.ID)
			if gerr != nil || fresh == nil {
				k.logger.Error("requeue re-read failed", "process", proc.ID, "cause", cause, "error", gerr)
				return goerr.Wrap(gerr, "requeue re-read", goerr.V("process", proc.ID))
			}
			if fresh.Status.Terminal() {
				// Another path finalized it. Nothing is owed, and the row is settled.
				return nil
			}
			if fresh.LeaseToken != fenceToken {
				return goerr.Wrap(ErrConflict, "lost the lease before requeue",
					goerr.V("process", proc.ID)) // never rebase, D50.
			}
			// The metrics were not folded, so they are still owed to the retry.
			proc = fresh
			continue
		}
		if err != nil {
			k.logger.Error("requeue failed", "process", proc.ID, "cause", cause, "error", err)
			return goerr.Wrap(err, "requeue", goerr.V("process", proc.ID))
		}
		return nil
	}
}

// release yields the Process (MaxStepsPerClaim consumed) back to pending. It
// re-reads first: after the last transition commit, the caller's proc holds the
// pre-commit Rev, so a CAS from that stale value would always conflict and leave
// the Process stuck running until the lease expires (#3).
// A conflict is retried from a fresh read rather than logged and dropped: the
// row would otherwise stay `running` with a lease nobody renews, and the next
// claim would charge the takeover as an unclean reclaim (ADR-0015) even though
// this worker exited in an orderly way.
//
// It returns nil only when the row really is back at pending, so the caller can
// tell ClaimReleased from ClaimAbandoned.
func (k *Kernel) release(ctx context.Context, proc *Process, fenceToken string) error {
	for {
		fresh, err := k.repo.GetProcess(ctx, proc.ID)
		if err != nil || fresh == nil {
			return goerr.Wrap(err, "release re-read", goerr.V("process", proc.ID))
		}
		if fresh.LeaseToken != fenceToken || fresh.Status != ProcessRunning {
			// Lease lost, or already moved off running by another path. Either way
			// this worker did not put it back.
			return goerr.Wrap(ErrConflict, "no longer this claim's row to release",
				goerr.V("process", proc.ID), goerr.V("status", fresh.Status))
		}
		p := fresh.clone()
		p.Status = ProcessPending
		// A released Process is runnable now, so it carries no wake time. The
		// Continue commit already cleared it; setting it here states the
		// post-condition rather than relying on that.
		p.WakeAt = nil
		p.LeaseUntil = nil
		p.UpdatedAt = k.clock()
		err = k.repo.Apply(ctx, ChangeSet{Processes: []*Process{p}})
		if errors.Is(err, ErrConflict) {
			continue // someone moved the row; re-read and decide again.
		}
		if err != nil {
			k.logger.Error("release failed", "process", proc.ID, "error", err)
			return goerr.Wrap(err, "release", goerr.V("process", proc.ID))
		}
		return nil
	}
}

// expireDueAwaits handles awaits past their deadline at claim time: timer ->
// responded {"fired":true}; question -> expired. It is fenced by the Process Rev
// so it serializes with a concurrent Respond.
func (k *Kernel) expireDueAwaits(ctx context.Context, proc *Process) {
	awaits, err := k.repo.ListAwaits(ctx, proc.ID)
	if err != nil {
		return
	}
	now := k.clock()
	var changed []*Await
	for _, aw := range awaits {
		if aw.Status != AwaitOpen || aw.Deadline == nil || aw.Deadline.After(now) {
			continue
		}
		switch aw.Kind {
		case AwaitTimer:
			aw.Fired = true
			aw.Status = AwaitResponded
			aw.Response = []byte(`{"fired":true}`)
			aw.RespondedAt = &now
			changed = append(changed, aw)
		case AwaitQuestion:
			aw.Status = AwaitExpired
			aw.RespondedAt = &now
			changed = append(changed, aw)
		}
	}
	if len(changed) == 0 {
		return
	}
	p := proc.clone()
	p.UpdatedAt = now
	_ = k.repo.Apply(ctx, ChangeSet{Processes: []*Process{p}, Awaits: changed})
}
