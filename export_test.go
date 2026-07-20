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
	return state, DecisionView{Kind: d.kind, Output: d.output, Typed: d.typed, Failure: d.failure, Awaits: d.awaits}, err
}

// FinishForTest drives the erased finish closure.
func (b StrategyBinding) FinishForTest(ctx context.Context, pid ProcessID, status ProcessStatus, typedOut any, f *Failure) error {
	return b.finish(ctx, pid, status, typedOut, f)
}

// DecisionView exposes a Decision's private fields for testing.
type DecisionView struct {
	Kind    DecisionKind
	Output  []byte
	Typed   any
	Failure *Failure
	Awaits  []AwaitSpec
}

// ViewDecision returns the private fields of a Decision. Output stays empty:
// the bytes only exist after EncodeOutput runs inside the step closure, so use
// StepForTest to observe them.
func ViewDecision[O any](d Decision[O]) DecisionView {
	v := DecisionView{Kind: d.kind, Failure: d.failure, Awaits: d.awaits}
	if d.hasOut {
		v.Typed = d.out
	}
	return v
}

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
