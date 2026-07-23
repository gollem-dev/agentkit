package agentkit

import (
	"context"
	"sync"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// Registry maps agent name -> type-erased binding. Register completes before
// Spawn/Serve start; thereafter the Registry is read-only.
type Registry struct {
	mu       sync.RWMutex
	bindings map[AgentName]StrategyBinding
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{bindings: make(map[AgentName]StrategyBinding)}
}

// FinishResult is the terminal outcome handed to a completion handler.
// Exactly one of Output / Failure is non-nil, or neither when cancelled.
type FinishResult[O any] struct {
	// Status is the committed terminal status: ProcessSucceeded, ProcessFailed
	// or ProcessCancelled. Never a non-terminal status.
	Status ProcessStatus
	// Output is the value passed to Done. Non-nil if and only if
	// Status == ProcessSucceeded.
	Output *O
	// Failure is the recorded failure. Non-nil if and only if
	// Status == ProcessFailed.
	Failure *Failure
}

// FinishHandler runs after a Process reaches a terminal state and that state
// has been committed. Delivery is best-effort: it never fires twice, but a
// crash between the commit and the call loses it entirely (ADR-0014).
type FinishHandler[O any] func(ctx context.Context, pid ProcessID, res FinishResult[O]) error

// RegisterOption configures Register.
type RegisterOption[O any] func(*registerConfig[O])

type registerConfig[O any] struct {
	onFinish    FinishHandler[O]
	onFinishSet bool // distinguishes "not given" from "given as nil".
	// historyRepo, when non-nil, opts this agent into runtime-managed History
	// persistence. It is O-independent, so no type-erased closure is needed.
	historyRepo gollem.HistoryRepository
}

// WithOnFinish wires a completion handler for this agent. The handler runs
// synchronously on whichever instance committed the terminal transition, after
// the commit succeeded; its error and panic are logged and change nothing. A
// nil handler yields ErrInvalidAgentDef.
func WithOnFinish[O any](h FinishHandler[O]) RegisterOption[O] {
	return func(c *registerConfig[O]) { c.onFinish, c.onFinishSet = h, true }
}

// WithHistoryRepository opts this agent into runtime-managed conversation
// History persistence, enabling sys.SessionGenerate / sys.SessionHistory. When
// set, the worker lazily loads History (keyed by the ProcessID string) on first
// use and saves it to hr before each transition commit — including terminal
// commits, so a later restart/handoff can read the final transcript (ADR-0017).
// Without it, SessionGenerate/SessionHistory return ErrHistoryNotConfigured (the
// managed conversation is not silently run without persistence); a strategy that
// manages History itself uses the primitive Generate instead. The store is a
// SEPARATE port (blob storage) from the Kernel's Repository, injected here per
// agent rather than on the Kernel.
func WithHistoryRepository[O any](hr gollem.HistoryRepository) RegisterOption[O] {
	return func(c *registerConfig[O]) { c.historyRepo = hr }
}

// Register registers a typed strategy and returns a typed handle carrying the
// input type I (D26/D43: required args are positional; the old AgentDef struct
// is gone). Empty name, version < 1, nil strategy, a nil completion handler, or
// a duplicate name yields ErrInvalidAgentDef. Contract: complete all Register
// calls before Spawn/Serve.
//
// The handle stays Agent[I]: O is consumed only by the completion handler, so
// carrying it on the handle would make it a phantom type parameter.
func Register[S, I, O any](r *Registry, name AgentName, version int, s Strategy[S, I, O], opts ...RegisterOption[O]) (Agent[I], error) {
	if name == "" || version < 1 || any(s) == nil {
		return Agent[I]{}, goerr.Wrap(ErrInvalidAgentDef, "name/version/strategy",
			goerr.V("name", name), goerr.V("version", version))
	}
	var cfg registerConfig[O]
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.onFinishSet && cfg.onFinish == nil {
		return Agent[I]{}, goerr.Wrap(ErrInvalidAgentDef, "nil finish handler", goerr.V("name", name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.bindings[name]; dup {
		return Agent[I]{}, goerr.Wrap(ErrInvalidAgentDef, "duplicate agent name", goerr.V("name", name))
	}
	r.bindings[name] = BindStrategy(s, opts...)
	return Agent[I]{name: name}, nil
}

// binding looks up a registered binding. Absent -> ErrUnknownAgent.
func (r *Registry) binding(name AgentName) (StrategyBinding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.bindings[name]
	if !ok {
		return StrategyBinding{}, goerr.Wrap(ErrUnknownAgent, "no such agent", goerr.V("agent", name))
	}
	return b, nil
}

// Agent is an opaque handle carrying the launch input type I (the return of
// Register). It is the only entry point for spawning a Process; the any-typed
// type erasure is confined to unexported methods (D43).
type Agent[I any] struct {
	name AgentName
}

// Name returns the agent name stored in Process.Agent.
func (a Agent[I]) Name() AgentName { return a.name }

// Spawn launches a Process from the application (typed; input is checked at
// compile time). It creates the Process pending and returns its ID immediately
// (asynchronous launch; execution starts at a worker's claim).
func (a Agent[I]) Spawn(ctx context.Context, k *Kernel, input I, opts ...SpawnOption) (ProcessID, error) {
	return k.spawnFromApp(ctx, a.name, input, opts...)
}

// SpawnChild launches a child Process from a strategy (journaled through sys,
// typed). The child insert is buffered into the transition commit (D48).
func (a Agent[I]) SpawnChild(ctx context.Context, sys Syscalls, input I, opts ...SpawnOption) (ProcessID, error) {
	return sys.spawn(ctx, a.name, input, opts...)
}
