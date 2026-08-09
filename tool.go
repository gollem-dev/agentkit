package agentkit

import (
	"context"

	"github.com/gollem-dev/gollem"
)

// ToolFactory is called once per claim to build the set of tools (gollem.Tool)
// a Process may use. It is a function type (not an interface) because per-claim
// construction is the main use and a closure is the most natural form; stateful
// implementations pass a method value. The implementation decides which tools
// to give based on proc.Agent / proc.Metadata (the agent kind itself is the
// selector — the kernel has no selection vocabulary). Process-independent
// dependencies can be injected via the ctx passed to Serve. The kernel does not
// interpret proc.
//
// Tools are used as-is: agentkit has no Tool wrapper and no SideEffect class.
// Side-effect idempotency and any fail-closed authorization are the tool
// author's responsibility (see the human confirmation pattern; the kernel has
// no approval gate).
type ToolFactory func(ctx context.Context, proc *Process) ([]gollem.Tool, error)

// MeteredTool is a gollem.Tool that also reports what its own Run consumed, in
// counters the caller defined with DefineMetricKey. Implementing it is optional:
// a plain tool still counts as one tool_calls and nothing else, which is all the
// kernel can see from the outside.
//
// It exists because a tool's cost is knowledge only the tool has — the tokens an
// embedded model call spent, the bytes it downloaded, the credits an upstream
// API charged — and a strategy has no way to reach inside it. A strategy counts
// what it knows itself through Syscalls.Count; the two feed the same counters.
type MeteredTool interface {
	gollem.Tool

	// Metered reports what the Run that just returned consumed. The kernel calls
	// it once after every Run, including one that returned an error — a call
	// that failed halfway may still have spent something — and folds the result
	// into Metrics alongside the tool_calls it already counted.
	//
	// The arguments are that Run's own call, result and error, so a tool that
	// holds no state between calls can still compute the figure. Returning nil
	// means "nothing to report". An entry with a nil key or a negative value is
	// dropped and logged; the rest are still counted, because the effect has
	// already happened and a bug in the accounting is no reason to discard it.
	//
	// It must not block or perform I/O: it runs inside the transition, on the
	// same hot path as Limit.
	Metered(call gollem.FunctionCall, result map[string]any, runErr error) map[MetricKey]int64
}
