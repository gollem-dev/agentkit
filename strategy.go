package agentkit

import (
	"context"

	"github.com/m-mizutani/goerr/v2"
)

// Strategy is a checkpointable typed state machine. S is the strategy state
// type, I is the launch input type.
type Strategy[S, I any] interface {
	// Version is the current state schema version. Reading older versions is
	// DecodeState's job (it absorbed the old Migrate).
	Version() int
	// Init builds the initial state, purely. Input is received typed (Spawn
	// passes the typed value through as any and BindStrategy's closure
	// type-checks it — no serialization runs). Init receives no Syscalls and no
	// ctx, so there is structurally no path to effects. It runs inside
	// Agent[I].Spawn / SpawnChild and the initial state is persisted with the
	// Process insert (never on the transition machine, so free of at-least-once
	// re-execution). Its error is returned synchronously by Spawn.
	Init(input I) (S, error)
	// Step runs one transition. It always receives the DecodeState-restored
	// state (the first transition too, since Init's result was persisted at
	// insert). The input I is folded into State and does not appear here.
	Step(ctx context.Context, sys Syscalls, state S) (S, Decision, error)
	// EncodeState / DecodeState fully own state serialization — the format is
	// free (JSON/gob/protobuf/...). agentkit only stores the bytes.
	EncodeState(state S) ([]byte, error)
	DecodeState(version int, raw []byte) (S, error)
}

// StrategyBinding is the type-erased form of a Strategy, storable in a Registry.
// agentkit itself never touches S or I.
type StrategyBinding struct {
	version int
	init    func(input any) (any, error)
	step    func(ctx context.Context, sys Syscalls, state any) (any, Decision, error)
	encode  func(state any) ([]byte, error)
	decode  func(version int, raw []byte) (any, error)
}

// BindStrategy erases the type of a Strategy by folding Init/Step/EncodeState/
// DecodeState into closures. Exported for building fake strategies in tests.
func BindStrategy[S, I any](s Strategy[S, I]) StrategyBinding {
	return StrategyBinding{
		version: s.Version(),
		init: func(in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, goerr.Wrap(ErrInvalidRequest, "spawn input type mismatch")
			}
			return s.Init(typed)
		},
		step: func(ctx context.Context, sys Syscalls, st any) (any, Decision, error) {
			state, dec, err := s.Step(ctx, sys, st.(S))
			return state, dec, err
		},
		encode: func(st any) ([]byte, error) { return s.EncodeState(st.(S)) },
		decode: func(v int, raw []byte) (any, error) { return s.DecodeState(v, raw) },
	}
}
