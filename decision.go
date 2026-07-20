package agentkit

import "time"

// DecisionKind is the outcome of one transition.
type DecisionKind string

const (
	DecisionContinue DecisionKind = "continue"
	DecisionSuspend  DecisionKind = "suspend"
	DecisionDone     DecisionKind = "done"
	DecisionFail     DecisionKind = "fail"
)

// Decision is the result of one transition, carrying the strategy's output
// type O. Its fields are unexported: it can only be built by Continue,
// Suspend, Fail or Done, so a Done without an output cannot be constructed.
//
// Go infers a type argument from a call's arguments only, never from the
// return or assignment context. Done therefore infers O from its argument,
// while Continue/Suspend/Fail need it written out: agentkit.Continue[MyOut]().
type Decision[O any] struct {
	kind    DecisionKind
	out     O           // Done: the typed output.
	hasOut  bool        // true only for Done.
	failure *Failure    // Fail.
	awaits  []AwaitSpec // Suspend: the declared waits.
}

// Continue advances to the next transition.
func Continue[O any]() Decision[O] { return Decision[O]{kind: DecisionContinue} }

// Done finalizes the Process as succeeded with the given output. The value is
// turned into the persisted bytes by the strategy's EncodeOutput, and the same
// value is handed to a completion handler without a round trip (ADR-0014).
func Done[O any](output O) Decision[O] {
	return Decision[O]{kind: DecisionDone, out: output, hasOut: true}
}

// Fail finalizes the Process as failed.
func Fail[O any](code FailureCode, message string) Decision[O] {
	return Decision[O]{kind: DecisionFail, failure: &Failure{Code: code, Message: message}}
}

// Suspend is a checkpoint that declares waits. specs are upserted on the
// transition commit. If an open wait already exists (e.g. declared in a prior
// transition), specs-less Suspend() is legal. If no open wait exists at commit
// time and no WaitChildren elision applies, the transition errors with
// ErrSuspendWithoutAwait. Re-declaring a non-open key is a no-op (idempotent
// re-execution after a restart).
func Suspend[O any](specs ...AwaitSpec) Decision[O] {
	return Decision[O]{kind: DecisionSuspend, awaits: specs}
}

// decision is the type-erased form of Decision[O], carried through the worker
// and the Step middleware chain, which know nothing of O. typed is the value
// Done received; the worker turns it into output via the binding's
// encodeOutput once the chain has settled on a Decision, and hands the same
// typed value to a completion handler so no decode is needed.
type decision struct {
	kind    DecisionKind
	output  []byte
	typed   any
	hasOut  bool // true only for Done; typed alone cannot say so for a nil-able O.
	failure *Failure
	awaits  []AwaitSpec
}

// erase drops O so the worker and the middleware chain can carry the Decision.
func (d Decision[O]) erase() decision {
	e := decision{kind: d.kind, failure: d.failure, awaits: d.awaits, hasOut: d.hasOut}
	if d.hasOut {
		e.typed = d.out
	}
	return e
}

// restore is erase's inverse, for a middleware reading the Decision back out.
// ok is false when the erased Decision was produced for a different O.
func restore[O any](e decision) (Decision[O], bool) {
	d := Decision[O]{kind: e.kind, failure: e.failure, awaits: e.awaits}
	if !e.hasOut {
		return d, true
	}
	out, ok := e.typed.(O)
	if !ok {
		return Decision[O]{}, false
	}
	d.out, d.hasOut = out, true
	return d, true
}

// AwaitSpec is a declared wait. It can only be built via the constructors
// below (three kinds only; confirmation is expressed as a question).
type AwaitSpec struct {
	key      AwaitKey
	kind     AwaitKind
	payload  []byte      // question payload.
	deadline *time.Time  // question deadline.
	children []ProcessID // children.
}

// AwaitOption configures an AwaitSpec.
type AwaitOption func(*awaitConfig)

type awaitConfig struct {
	deadline *time.Time
}

// WithDeadline sets a Question deadline. Reaching it makes the await expired.
func WithDeadline(t time.Time) AwaitOption {
	return func(c *awaitConfig) { c.deadline = &t }
}

// Question declares a wait for a human answer. Encoding of the payload is the
// caller's (a confirmation sends e.g. []byte("...question...") and the answer
// is []byte("yes"|"no")).
func Question(key AwaitKey, payload []byte, opts ...AwaitOption) AwaitSpec {
	var cfg awaitConfig
	for _, o := range opts {
		o(&cfg)
	}
	return AwaitSpec{key: key, kind: AwaitQuestion, payload: payload, deadline: cfg.deadline}
}

// Timer declares a wait until a time.
func Timer(key AwaitKey, until time.Time) AwaitSpec {
	return AwaitSpec{key: key, kind: AwaitTimer, deadline: &until}
}

// WaitChildren declares a wait for all the given child Processes to finish.
func WaitChildren(key AwaitKey, children ...ProcessID) AwaitSpec {
	return AwaitSpec{key: key, kind: AwaitChildren, children: children}
}
