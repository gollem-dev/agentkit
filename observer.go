package agentkit

import (
	"context"

	"github.com/gollem-dev/gollem"
)

// EffectContext identifies "which Process's which transition produced this
// effect". It can key an audit row.
type EffectContext struct {
	ProcessID ProcessID
	RootID    ProcessID // correlation id for the whole tree (reaches down to children).
	Agent     AgentName
	StateSeq  int // transition number.
}

// Observer is an observation hook for effects (LLM calls, tool calls, spawns).
// It is a best-effort side-channel for audit trails, distributed tracing, and
// metrics transport, injected via WithObserver.
//
//   - The framework does NOT persist anything here (this is not a journal).
//   - Observation, not control: the callbacks cannot stop execution (deny is
//     the strategy/tool's job).
//   - Effects run at-least-once (they can re-execute on replay), so the hooks
//     fire on every execution including re-runs — recording what actually
//     happened, faithfully for audit.
//   - Each field is a span: called at the start of the effect, returning a
//     callback invoked at completion. Record intent at start, result/error at
//     completion. A nil field is not called.
//   - The framework recovers panics from observer calls and only logs (an audit
//     bug must not kill a transition).
//   - The framework passes deep copies (or immutable data) of input/args/child
//     so the observer cannot mutate what execution uses.
//   - The Spawn completion callback fires AFTER the child is persisted by the
//     transition commit (the start callback fires at spawn-request time); on a
//     failed commit the completion is called with the error (or not at all), so
//     the audit matches whether a child was actually created.
type Observer struct {
	Generate func(ctx context.Context, ec EffectContext, input []gollem.Input, role ModelRole) func(res *GenerateResult, err error)
	ToolCall func(ctx context.Context, ec EffectContext, call gollem.FunctionCall) func(res map[string]any, err error)
	Spawn    func(ctx context.Context, ec EffectContext, child ProcessID, agent AgentName) func(err error)
}
