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
