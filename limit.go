package agentkit

import (
	"context"
	"fmt"

	"github.com/m-mizutani/goerr/v2"
)

// LimitKind is which of the three verdicts a LimitDecision carries.
//
// The constants carry the full type name as their prefix, unlike AwaitKind's
// AwaitQuestion or DecisionKind's DecisionContinue. Those get away with the
// short form because their constructors are named differently (Question,
// Continue); here LimitPass / LimitNotice / LimitStop are the constructors.
type LimitKind string

const (
	// LimitKindPass continues with nothing to report.
	LimitKindPass LimitKind = "pass"
	// LimitKindNotice continues and carries a message for the strategy.
	LimitKindNotice LimitKind = "notice"
	// LimitKindStop refuses to continue and carries the reason.
	LimitKindStop LimitKind = "stop"
)

// LimitDecision is a Limit verdict: one kind, plus the message that goes
// with it. Build it with LimitPass, LimitNotice or LimitStop.
//
// It is both what Strategy.Limit returns and what a strategy observes through
// Syscalls.LimitStatus(), so a field added here reaches readers without
// changing any signature. The message is one field rather than a separate
// reason and notice because a decision is exactly one kind: "stopped, and also
// here is an unrelated notice" is not a state that exists.
//
// The kernel never interprets the message. It moves it to whoever asked for it
// (ADR-0011).
type LimitDecision struct {
	kind    LimitKind
	message string
}

// LimitPass continues with nothing to report.
func LimitPass() LimitDecision { return LimitDecision{kind: LimitKindPass} }

// LimitNotice continues but attaches a message for the strategy, which may read
// it through Syscalls.LimitStatus() and act on it — put it in a prompt, drop
// expensive tools, wrap up early. agentkit itself does none of that: a decision
// nobody reads costs nothing beyond the Limit call.
//
// An empty message is a LimitPass, so a LimitKindNotice always carries text and
// a reader never has to test for both.
func LimitNotice(msg string) LimitDecision {
	if msg == "" {
		return LimitPass()
	}
	return LimitDecision{kind: LimitKindNotice, message: msg}
}

// LimitStop refuses to continue. Before an effect the reason reaches the
// strategy wrapped in ErrLimitExceeded; at a transition boundary it becomes the
// Failure message of a failed(limit_exceeded) Process.
//
// An empty reason is replaced, because Failure.Message must say something.
func LimitStop(reason string) LimitDecision {
	if reason == "" {
		reason = "limit exceeded"
	}
	return LimitDecision{kind: LimitKindStop, message: reason}
}

// Kind reports which verdict this is. The zero LimitDecision reads as
// LimitKindPass, so a reader holding one taken before any verdict was recorded
// needs no special case.
func (d LimitDecision) Kind() LimitKind {
	if d.kind == "" {
		return LimitKindPass
	}
	return d.kind
}

// Message is the text Limit attached: the notice for LimitKindNotice, the
// reason for LimitKindStop, and "" for LimitKindPass.
func (d LimitDecision) Message() string { return d.message }

// Limiter decides whether a Process may continue. Measurement (Metrics) is the
// Kernel's job; the decision is the strategy's, expressed as Strategy.Limit,
// whose shape this type is (ADR-0010). It is also the argument type the bundled
// strategies take to build that method from a caller's closure.
//
// It runs at three points: at each transition boundary, before every Generate,
// CallTool and SpawnChild, and again after each of those has been metered. The
// first two refuse the work when the verdict says stop; the third cannot — the
// effect has run — and only updates what Syscalls.LimitStatus() reports.
//
// metrics is a snapshot of "committed cumulative (proc.Metrics) plus what this
// run has accumulated so far", so an effect cannot consume budget without the
// next call seeing it.
//
// Two obligations follow from being called that often, roughly 1 + 2×effects
// times per attempt:
//
//   - It must be READ-ONLY with respect to whatever it consults. A call is an
//     enquiry, not an acquisition: the same effect is asked about more than
//     once, and once with the work already done. A Limiter that draws a token
//     from a rate limiter, or charges a quota, charges several times per effect
//     and refuses work nobody performed. Consult the current state instead and
//     leave the accounting to whatever owns it.
//   - It must be cheap and NON-BLOCKING. It is on the transition hot path and
//     the claim holds its lease throughout, so waiting here turns a throttle
//     into a lease expiry and an unclean reclaim (ADR-0010, ADR-0015). Work that
//     has to wait belongs behind a timer await.
type Limiter func(ctx context.Context, proc *Process, metrics Metrics) LimitDecision

// callLimit runs a strategy's Limit at the transition boundary, where
// runTransition's recover is not yet in scope. Limit is strategy-author code, so
// a panic there would otherwise take the worker goroutine down; it is converted
// into a transition error carrying the same "strategy panic" message
// runTransition produces, discriminated by the hook key.
//
// The other two call sites (checkLimit and meter) run inside runTransition and
// are already covered. Wrapping them here too would not add protection; it would
// change what the strategy sees. A recovered panic there would come back as the
// syscall's own error, which a strategy may catch and carry on past — turning a
// strategy panic into something other than a transition error, which is the one
// meaning it has everywhere else.
func callLimit(ctx context.Context, f Limiter, proc *Process, m Metrics) (d LimitDecision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = goerr.New("strategy panic",
				goerr.V("panic", fmt.Sprint(r)), goerr.V("hook", "Limit"))
		}
	}()
	return f(ctx, proc, m), nil
}
