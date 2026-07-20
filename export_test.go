package agentkit

import (
	"context"
	"time"

	"github.com/gollem-dev/gollem"
)

// ResolveModel exposes resolveModel for testing role resolution.
func (k *Kernel) ResolveModel(role ModelRole) gollem.LLMClient { return k.resolveModel(role) }

// Exposed StrategyBinding closures for white-box testing of BindStrategy.
func (b StrategyBinding) InitForTest(in any) (any, error)              { return b.init(in) }
func (b StrategyBinding) EncodeForTest(st any) ([]byte, error)         { return b.encode(st) }
func (b StrategyBinding) DecodeForTest(v int, raw []byte) (any, error) { return b.decode(v, raw) }
func (b StrategyBinding) VersionForTest() int                          { return b.version }

// HasFinishForTest reports whether a completion handler was registered.
func (b StrategyBinding) HasFinishForTest() bool { return b.finish != nil }

// StepForTest drives the erased step closure and reports the erased decision.
func (b StrategyBinding) StepForTest(ctx context.Context, sys Syscalls, st any) (any, DecisionView, error) {
	state, d, err := b.step(ctx, sys, st)
	return state, DecisionView{
		Kind: d.kind, Output: d.output, Typed: d.typed, HasOut: d.hasOut,
		Failure: d.failure, Awaits: d.awaits,
	}, err
}

// EncodeOutputForTest drives the erased output encoder.
func (b StrategyBinding) EncodeOutputForTest(typedOut any) ([]byte, error) {
	return b.encodeOutput(typedOut)
}

// FinishForTest drives the erased finish closure.
func (b StrategyBinding) FinishForTest(ctx context.Context, pid ProcessID, status ProcessStatus, typedOut any, f *Failure) error {
	return b.finish(ctx, pid, status, typedOut, f)
}

// DecisionView exposes a Decision's private fields for testing. Typed is the
// erased envelope, so compare it against WrapOutputForTest(want).
type DecisionView struct {
	Kind    DecisionKind
	Output  []byte
	Typed   any
	HasOut  bool
	Failure *Failure
	Awaits  []AwaitSpec
}

// ViewDecision returns the private fields of a Decision. Output stays empty:
// the bytes only exist after the worker runs EncodeOutput, past the Step
// middleware chain.
func ViewDecision[O any](d Decision[O]) DecisionView {
	e := d.erase()
	return DecisionView{Kind: e.kind, Typed: e.typed, HasOut: e.hasOut, Failure: e.failure, Awaits: e.awaits}
}

// WrapOutputForTest builds the erased envelope an output travels in, so a test
// can state the expected value without reaching into unexported types.
func WrapOutputForTest[O any](out O) any { return typedOutput[O]{value: out} }

// This file exposes internal helpers for white-box testing (package agentkit),
// consumed by the black-box test package agentkit_test.

// AddMetrics is the exported form of addMetrics for testing.
func AddMetrics(a, b Metrics) Metrics { return addMetrics(a, b) }

// CloneProcess is the exported form of (*Process).clone for testing.
func CloneProcess(p *Process) *Process { return p.clone() }

// AwaitSpecView exposes an AwaitSpec's private fields for testing.
type AwaitSpecView struct {
	Key      AwaitKey
	Kind     AwaitKind
	Payload  []byte
	Deadline *time.Time
	Children []ProcessID
}

// ViewAwaitSpec returns the private fields of an AwaitSpec.
func ViewAwaitSpec(s AwaitSpec) AwaitSpecView {
	return AwaitSpecView{
		Key:      s.key,
		Kind:     s.kind,
		Payload:  s.payload,
		Deadline: s.deadline,
		Children: s.children,
	}
}
