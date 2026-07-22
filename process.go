package agentkit

import (
	"maps"
	"time"
)

// ProcessStatus is the lifecycle state of a Process. The proposal's
// limit_exceeded status is dropped; it is expressed as failed + FailureCode.
type ProcessStatus string

const (
	ProcessPending   ProcessStatus = "pending"
	ProcessRunning   ProcessStatus = "running"
	ProcessWaiting   ProcessStatus = "waiting"
	ProcessSucceeded ProcessStatus = "succeeded"
	ProcessFailed    ProcessStatus = "failed"
	ProcessCancelled ProcessStatus = "cancelled"
)

// Terminal reports whether the status is a final state (no further transitions).
func (s ProcessStatus) Terminal() bool {
	switch s {
	case ProcessSucceeded, ProcessFailed, ProcessCancelled:
		return true
	default:
		return false
	}
}

// FailureCode categorizes why a Process failed.
type FailureCode string

const (
	// FailureStrategyError: Decision=Fail or an unrecoverable Step error.
	FailureStrategyError FailureCode = "strategy_error"
	// FailureLimitExceeded: stopped by a Limiter.
	FailureLimitExceeded FailureCode = "limit_exceeded"
	// FailureRetryExhausted: step retry limit exceeded.
	FailureRetryExhausted FailureCode = "retry_exhausted"
	// FailureUncleanReclaim: too many claims died mid-transition. This is a
	// worker-health signal rather than a strategy bug, which is why it is
	// distinct from FailureRetryExhausted — the cause and the remedy differ.
	FailureUncleanReclaim FailureCode = "unclean_reclaim"
)

// AttemptInfo reports prior attempts at the current transition that did not
// commit. A zero value means this is the first attempt.
type AttemptInfo struct {
	// Errors is the number of previous attempts that returned an error.
	// Effects up to the error point may have fired.
	Errors int
	// UncleanReclaims is the number of previous claims that died
	// mid-transition. Nothing is known about how far they got: the transition
	// may have completed every effect and died before commit, and a
	// lease-expiry reclaim may overlap a still-running predecessor.
	UncleanReclaims int
}

// IsReplay reports whether a previous attempt at this transition may have
// executed effects.
func (a AttemptInfo) IsReplay() bool { return a.Errors > 0 || a.UncleanReclaims > 0 }

// Failure describes a failed Process.
type Failure struct {
	Code    FailureCode `json:"code"`
	Message string      `json:"message"`
}

// SubjectRef is the target of turn-lock (single-flight) suppression.
type SubjectRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Process is the complete persisted representation of an execution unit. It is
// the aggregate the Repository stores.
type Process struct {
	ID       ProcessID
	Agent    AgentName
	Status   ProcessStatus
	Metadata map[string]string // optional Spawn WithMetadata. Infrastructure-facing
	// process scope (e.g. "tenant"->"acme") for ToolFactory. The kernel does not
	// interpret it, and it is NOT strategy input. It is not a credential
	// (WithMetadata callers must derive it server-side from a validated principal).
	Output       []byte   // non-nil when succeeded. Consumed by the parent's children Await and GetProcess.
	Failure      *Failure // non-nil when failed.
	State        []byte   // EncodeState output, stored verbatim (the kernel never converts it).
	StateVersion int      // strategy version that wrote State (the first arg to DecodeState).
	StateSeq     int      // number of committed transitions. 0 = first Step not yet committed.
	StepAttempts int      // failure count of the current transition (reset to 0 on a successful commit).
	// UncleanReclaims counts claims that took over this Process after its
	// previous claim died mid-transition. Same reset scope as StepAttempts.
	// Maintained by ClaimNextProcess (see the Repository contract), never by the
	// worker — the worker only reads it to bound re-execution. Eager dispatch
	// (claimSpecific) claims only pending rows, so it never increments this;
	// reclaiming an expired-lease running row is ClaimNextProcess's job alone.
	UncleanReclaims int
	Metrics         Metrics // committed cumulative usage.
	ParentID        *ProcessID
	RootID          ProcessID // self if no parent.
	Subject         *SubjectRef
	IdempotencyKey  string
	CancelRequested bool
	CancelReason    string
	WakeAt          *time.Time // wake time while waiting (min of open await deadlines).
	LeaseOwner      string     // diagnostic (hostname/uuid). Not used for fencing (shared across WithPollConcurrency claims).
	LeaseToken      string     // unique per claim (ClaimNextProcess and eager claimSpecific each mint a uuid v7). The fence identity: a worker
	// keeps its claim's LeaseToken and, on conflict, checks "stored LeaseToken == mine" to tell "still hold the
	// lease (rebase ok)" from "re-claimed by another worker (abandon)".
	LeaseUntil *time.Time
	Rev        int64 // optimistic concurrency token. Incremented on every Apply/claim write. Also used to detect lease expiry.
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// clone returns a deep copy of the Process so callers can mutate a candidate
// without touching the original.
func (p *Process) clone() *Process {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Metadata != nil {
		cp.Metadata = maps.Clone(p.Metadata)
	}
	if p.Output != nil {
		cp.Output = append([]byte(nil), p.Output...)
	}
	if p.State != nil {
		cp.State = append([]byte(nil), p.State...)
	}
	if p.Failure != nil {
		f := *p.Failure
		cp.Failure = &f
	}
	if p.Subject != nil {
		s := *p.Subject
		cp.Subject = &s
	}
	if p.Metrics != nil {
		cp.Metrics = maps.Clone(p.Metrics)
	}
	if p.ParentID != nil {
		id := *p.ParentID
		cp.ParentID = &id
	}
	if p.WakeAt != nil {
		t := *p.WakeAt
		cp.WakeAt = &t
	}
	if p.LeaseUntil != nil {
		t := *p.LeaseUntil
		cp.LeaseUntil = &t
	}
	return &cp
}
