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
	workerID         string
	lease            time.Duration
	pollInterval     time.Duration
	maxStepsPerClaim int
	maxStepAttempts  int
	concurrency      int
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
func WithMaxStepsPerClaim(n int) ServeOption { return func(c *serveConfig) { c.maxStepsPerClaim = n } }

// WithMaxStepAttempts sets the step retry limit. Default: 3.
func WithMaxStepAttempts(n int) ServeOption { return func(c *serveConfig) { c.maxStepAttempts = n } }

// WithConcurrency sets the number of parallel claim loops. Default: 1.
func WithConcurrency(n int) ServeOption { return func(c *serveConfig) { c.concurrency = n } }

func newServeConfig(opts []ServeOption) serveConfig {
	host, _ := os.Hostname()
	cfg := serveConfig{
		workerID:         host + "/" + uuid.Must(uuid.NewV7()).String(),
		lease:            60 * time.Second,
		pollInterval:     500 * time.Millisecond,
		maxStepsPerClaim: 16,
		maxStepAttempts:  3,
		concurrency:      1,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}
	return cfg
}

// Serve runs claim loops until ctx is done (blocking). WithConcurrency loops
// share a workerID; the per-claim LeaseToken is the fence identity (D50).
func (k *Kernel) Serve(ctx context.Context, opts ...ServeOption) error {
	cfg := newServeConfig(opts)
	var wg sync.WaitGroup
	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k.serveLoop(ctx, cfg)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (k *Kernel) serveLoop(ctx context.Context, cfg serveConfig) {
	for ctx.Err() == nil {
		proc, err := k.repo.ClaimNextProcess(ctx, cfg.workerID, k.clock().Add(cfg.lease), k.clock())
		if err != nil {
			k.logger.Error("claim failed", "error", err)
			sleepOrDone(ctx, cfg.pollInterval)
			continue
		}
		if proc == nil {
			sleepOrDone(ctx, cfg.pollInterval)
			continue
		}
		k.runClaim(ctx, cfg, proc)
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

// runClaim drives one claimed Process for up to maxStepsPerClaim transitions.
func (k *Kernel) runClaim(ctx context.Context, cfg serveConfig, proc *Process) {
	claimToken := proc.LeaseToken // the fence identity for this claim (D50).
	b, err := k.agents.binding(proc.Agent)
	if err != nil {
		// Unknown agent: a permanent config mismatch (e.g. a binary generation skew).
		_ = k.finalize(ctx, proc, failWith(FailureStrategyError, err), claimToken, nil)
		return
	}
	var toolList []gollem.Tool
	if k.toolFactory != nil {
		toolList, err = k.toolFactory(ctx, proc)
		if err != nil {
			k.requeue(ctx, cfg, proc, goerr.Wrap(err, "tool factory"), nil)
			return
		}
	}
	k.expireDueAwaits(ctx, proc)

	for i := 0; i < cfg.maxStepsPerClaim; i++ {
		fresh, err := k.repo.GetProcess(ctx, proc.ID)
		if err != nil {
			return
		}
		if fresh.LeaseToken != claimToken {
			return // lost the lease between transitions.
		}
		if fresh.CancelRequested {
			_ = k.finalize(ctx, fresh, cancelledWith(fresh.CancelReason), claimToken, nil)
			return
		}
		proc = fresh
		if k.limiter != nil {
			if lerr := k.limiter(ctx, proc, proc.Metrics); lerr != nil {
				_ = k.finalize(ctx, proc, failWith(FailureLimitExceeded, lerr), claimToken, nil)
				return
			}
		}

		sys := newSyscalls(k, proc, toolList)
		rawState, dec, terr := k.runTransition(ctx, sys, b, proc)
		if terr != nil {
			// This transition did not commit; its buffered children are dropped.
			sys.notifySpawnDone(terr)
			if proc.StepAttempts+1 > cfg.maxStepAttempts {
				_ = k.finalize(ctx, proc, failWith(FailureRetryExhausted, terr), claimToken, sys.runMetrics)
			} else {
				k.requeue(ctx, cfg, proc, terr, sys.runMetrics)
			}
			return
		}

		if dec.kind == DecisionDone || dec.kind == DecisionFail {
			// commitTerminal fires the spawn OnCommit callbacks itself (nil on commit, err on abandon).
			_ = k.commitTerminal(ctx, proc, rawState, b.version, sys.seq, dec, sys, claimToken)
			return
		}

		cs, cerr := k.buildCommit(ctx, proc, rawState, b.version, sys.seq, dec, sys, cfg)
		if cerr != nil {
			// Suspend-without-await, invalid child ref, etc. -> retry path.
			sys.notifySpawnDone(cerr)
			if proc.StepAttempts+1 > cfg.maxStepAttempts {
				_ = k.finalize(ctx, proc, failWith(FailureRetryExhausted, cerr), claimToken, sys.runMetrics)
			} else {
				k.requeue(ctx, cfg, proc, cerr, sys.runMetrics)
			}
			return
		}
		if err := k.repo.Apply(ctx, cs.changeSet); err != nil {
			if errors.Is(err, ErrConflict) {
				sys.notifySpawnDone(err) // this attempt's buffered children did not commit.
				cur, gerr := k.repo.GetProcess(ctx, proc.ID)
				if gerr != nil || cur == nil || cur.LeaseToken != claimToken {
					return // lost the lease -> abandon (never rebase, D50).
				}
				proc = cur
				i--
				continue // same-lease race (Cancel etc.) -> rebuild.
			}
			sys.notifySpawnDone(err)
			return
		}
		// Committed: fire spawn OnCommit callbacks (#5/#8).
		sys.notifySpawnDone(nil)
		switch {
		case cs.suspend:
			return // waiting committed.
		case cs.elidedRunning:
			// WaitChildren elision: children already done; continue this claim.
			continue
		default:
			continue // Continue: next transition (loop re-reads fresh).
		}
	}
	k.release(ctx, proc, claimToken)
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
	return func(p *Process) { p.Status = ProcessFailed; p.Failure = &Failure{Code: code, Message: err.Error()} }
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
	p.StepAttempts = 0
	p.Metrics = addMetrics(p.Metrics, sys.runMetrics)
	p.Metrics = addMetrics(p.Metrics, Metrics{MetricSteps: 1})
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
				cs.Events = append(cs.Events, &Event{ProcessID: proc.ID, Type: EventAwaitCreated, Key: spec.key, At: now})
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
	return ChildResult{ProcessID: cp.ID, Status: cp.Status, Output: cp.Output, Failure: cp.Failure}
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
func (k *Kernel) commitTerminal(ctx context.Context, proc *Process, rawState []byte, version, seq int, dec decision, sys *syscalls, fenceToken string) error {
	return k.commitFinal(ctx, proc, fenceToken, func(p *Process) {
		p.State = rawState
		p.StateVersion = version
		p.StateSeq = seq
		p.StepAttempts = 0
		p.Metrics = addMetrics(p.Metrics, sys.runMetrics)
		p.Metrics = addMetrics(p.Metrics, Metrics{MetricSteps: 1})
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
	return k.commitFinal(ctx, proc, fenceToken, func(p *Process) {
		mut(p)
		if foldMetrics != nil {
			p.Metrics = addMetrics(p.Metrics, foldMetrics)
		}
	}, nil, nil)
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
const externalFence = ""

func (k *Kernel) commitFinal(ctx context.Context, proc *Process, fenceToken string, mutate func(*Process), sys *syscalls, typedOut any) error {
	for {
		now := k.clock()
		p := proc.clone()
		mutate(p)
		p.UpdatedAt = now
		p.WakeAt = nil
		p.LeaseUntil = nil

		cs := ChangeSet{Processes: []*Process{p}}
		cs.Events = append(cs.Events, &Event{ProcessID: p.ID, Type: EventProcessFinished, At: now})
		if sys != nil {
			cs.Processes = append(cs.Processes, sys.pendingChildren...)
			cs.Events = append(cs.Events, sys.pendingEvents...)
		}
		// Close this Process's open awaits.
		awaits, err := k.repo.ListAwaits(ctx, p.ID)
		if err != nil {
			return k.abortFinal(sys, goerr.Wrap(err, "list own awaits for finalize"))
		}
		for _, aw := range awaits {
			if aw.Status == AwaitOpen {
				aw.Status = AwaitCancelled
				cs.Awaits = append(cs.Awaits, aw)
			}
		}
		// Wake the parent if this completes an open children await.
		if p.ParentID != nil {
			if err := k.wakeParentIfComplete(ctx, *p.ParentID, p, &cs, now); err != nil {
				return k.abortFinal(sys, err)
			}
		}

		err = k.repo.Apply(ctx, cs)
		if errors.Is(err, ErrConflict) {
			fresh, gerr := k.repo.GetProcess(ctx, proc.ID)
			if gerr != nil || fresh == nil || fresh.Status.Terminal() {
				return k.abortFinal(sys, nil) // already terminal by another path.
			}
			if fenceToken == externalFence {
				return goerr.Wrap(ErrConflict, "external finalize lost a race; retry") // caller re-reads (#1).
			}
			if fresh.LeaseToken != fenceToken {
				return k.abortFinal(sys, nil) // worker lost the lease -> abandon (never rebase Rev, #2/D50).
			}
			proc = fresh
			continue
		}
		if err != nil {
			return k.abortFinal(sys, err)
		}
		if sys != nil {
			sys.notifySpawnDone(nil)
		}
		k.fireFinish(ctx, p, typedOut)
		return nil
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

// wakeParentIfComplete, on a child finalize, always includes the parent row as a
// CAS target when an open children await contains this child (serializing
// siblings on the parent Rev, closing the #3c write skew), and responds+wakes
// the parent when all children are terminal (with this child's terminal state
// overlaid). Any required read failure returns an error so the caller aborts the
// finalize (never a partial commit that would lose the wakeup, #2). A parent
// that no longer exists is treated as "nothing to wake".
func (k *Kernel) wakeParentIfComplete(ctx context.Context, parentID ProcessID, child *Process, cs *ChangeSet, now time.Time) error {
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
	touched := false
	pClone := parent.clone()
	for _, aw := range awaits {
		if aw.Kind != AwaitChildren || aw.Status != AwaitOpen || !containsID(aw.Children, child.ID) {
			continue
		}
		touched = true
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
			}
			cs.Awaits = append(cs.Awaits, aw)
		}
	}
	if touched {
		// Always CAS the parent row (a no-op write if not all terminal) so sibling
		// finalizes serialize on the parent Rev (#3c).
		cs.Processes = append(cs.Processes, pClone)
	}
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

// requeue puts the Process back to pending with a backoff, folding this run's
// metrics on the successful Apply (#5).
func (k *Kernel) requeue(ctx context.Context, cfg serveConfig, proc *Process, cause error, foldMetrics Metrics) {
	now := k.clock()
	p := proc.clone()
	p.Status = ProcessPending
	p.StepAttempts = proc.StepAttempts + 1
	// max(0, ...) keeps the shift count non-negative: the uint conversion this
	// replaces was what previously made a bogus stored StepAttempts harmless,
	// and a negative shift count panics at runtime.
	attempts := max(0, minInt(p.StepAttempts, 6))
	backoff := time.Duration(1<<attempts) * time.Second
	if backoff > 60*time.Second {
		backoff = 60 * time.Second
	}
	wake := now.Add(backoff)
	p.WakeAt = &wake
	p.LeaseUntil = nil
	p.UpdatedAt = now
	if foldMetrics != nil {
		p.Metrics = addMetrics(p.Metrics, foldMetrics)
	}
	if err := k.repo.Apply(ctx, ChangeSet{Processes: []*Process{p}}); err != nil {
		k.logger.Error("requeue failed", "process", proc.ID, "cause", cause, "error", err)
	}
}

// release yields the Process (MaxStepsPerClaim consumed) back to pending. It
// re-reads first: after the last transition commit, the caller's proc holds the
// pre-commit Rev, so a CAS from that stale value would always conflict and leave
// the Process stuck running until the lease expires (#3).
func (k *Kernel) release(ctx context.Context, proc *Process, fenceToken string) {
	fresh, err := k.repo.GetProcess(ctx, proc.ID)
	if err != nil || fresh == nil {
		return
	}
	if fresh.LeaseToken != fenceToken || fresh.Status != ProcessRunning {
		return // lease lost, or already moved off running by another path.
	}
	p := fresh.clone()
	p.Status = ProcessPending
	p.LeaseUntil = nil
	p.UpdatedAt = k.clock()
	if err := k.repo.Apply(ctx, ChangeSet{Processes: []*Process{p}}); err != nil {
		k.logger.Error("release failed", "process", proc.ID, "error", err)
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
