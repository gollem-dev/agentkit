package agentkit

import (
	"context"
	"sync"

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

// Register registers a typed strategy and returns a typed handle carrying the
// input type I (D26/D43: required args are positional; the old AgentDef struct
// is gone). Empty name, version < 1, nil strategy, or a duplicate name yields
// ErrInvalidAgentDef. Contract: complete all Register calls before Spawn/Serve.
func Register[S, I any](r *Registry, name AgentName, version int, s Strategy[S, I]) (Agent[I], error) {
	if name == "" || version < 1 || any(s) == nil {
		return Agent[I]{}, goerr.Wrap(ErrInvalidAgentDef, "name/version/strategy",
			goerr.V("name", name), goerr.V("version", version))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.bindings[name]; dup {
		return Agent[I]{}, goerr.Wrap(ErrInvalidAgentDef, "duplicate agent name", goerr.V("name", name))
	}
	r.bindings[name] = BindStrategy(s)
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
