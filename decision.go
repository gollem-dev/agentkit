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

// Decision is the result of one transition. The kernel only nil-checks the
// user data (Output); it never touches the content.
type Decision struct {
	Kind    DecisionKind
	Output  []byte      // Done: the output (encoding is the strategy author's).
	Failure *Failure    // Fail.
	Awaits  []AwaitSpec // Suspend: the declared waits.
}

// Continue advances to the next transition.
func Continue() Decision { return Decision{Kind: DecisionContinue} }

// Done finalizes the Process as succeeded with the given output. The kernel
// only checks that output is non-nil (E9); the format is the author's.
func Done(output []byte) Decision { return Decision{Kind: DecisionDone, Output: output} }

// Fail finalizes the Process as failed.
func Fail(code FailureCode, message string) Decision {
	return Decision{Kind: DecisionFail, Failure: &Failure{Code: code, Message: message}}
}

// Suspend is a checkpoint that declares waits. specs are upserted on the
// transition commit. If an open wait already exists (e.g. declared in a prior
// transition), specs-less Suspend() is legal. If no open wait exists at commit
// time and no WaitChildren elision applies, the transition errors with
// ErrSuspendWithoutAwait. Re-declaring a non-open key is a no-op (idempotent
// re-execution after a restart).
func Suspend(specs ...AwaitSpec) Decision {
	return Decision{Kind: DecisionSuspend, Awaits: specs}
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
