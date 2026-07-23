package agentkit

import (
	"context"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// Strategy is a checkpointable typed state machine. S is the strategy state
// type, I is the launch input type, O is the output type passed to Done.
type Strategy[S, I, O any] interface {
	// Version is the current state schema version. Reading older versions is
	// DecodeState's job (it absorbed the old Migrate).
	Version() int
	// Init builds the initial state, purely. Input is received typed (Spawn
	// passes the typed value through as any and BindStrategy's closure
	// type-checks it — no serialization runs). Init receives no Syscalls and no
	// ctx, so a STRATEGY AUTHOR has structurally no path to effects here.
	//
	// That guarantee is about this signature, not about the surrounding call.
	// Whoever configures the Kernel can wrap Init with InitMiddleware, which does
	// receive a ctx and can perform effects around it. Init still runs inside
	// Agent[I].Spawn / SpawnChild and never on the transition machine, so unlike
	// Step it is free of at-least-once re-execution — which makes it the safer
	// of the two places for such an effect. Its error is returned synchronously
	// by Spawn.
	Init(input I) (S, error)
	// Step runs one transition. It always receives the DecodeState-restored
	// state (the first transition too, since Init's result was persisted at
	// insert). The input I is folded into State and does not appear here.
	Step(ctx context.Context, sys Syscalls, state S) (S, Decision[O], error)
	// EncodeState / DecodeState fully own state serialization — the format is
	// free (JSON/gob/protobuf/...). agentkit only stores the bytes.
	EncodeState(state S) ([]byte, error)
	DecodeState(version int, raw []byte) (S, error)
	// EncodeOutput turns the value passed to Done into the bytes stored on
	// Process.Output. The output must be persisted because a parent reads it
	// through a children await (await.go), which crosses instance and time
	// boundaries. There is deliberately no DecodeOutput: nothing reads those
	// bytes back as O — a completion handler receives the value Done was given
	// (no round trip), and a parent treats a child's Output as opaque bytes.
	EncodeOutput(out O) ([]byte, error)
}

// StrategyBinding is the type-erased form of a Strategy, storable in a Registry.
// agentkit itself never touches S, I or O.
type StrategyBinding struct {
	version int
	init    func(input any) (any, error)
	step    func(ctx context.Context, sys Syscalls, state any) (any, decision, error)
	encode  func(state any) ([]byte, error)
	decode  func(version int, raw []byte) (any, error)
	// encodeOutput runs after the Step middleware chain, not inside step: a
	// StepMiddleware can replace the Decision (NewStepResult) and has no way to
	// encode, so the erased decision carries the typed value and the worker
	// encodes whichever Decision came out of the chain.
	encodeOutput func(typedOut any) ([]byte, error)
	// finish is nil unless WithOnFinish was given. typedOut is the value Done
	// received (nil for failed/cancelled).
	finish func(ctx context.Context, pid ProcessID, status ProcessStatus, typedOut any, f *Failure) error
	// historyRepo is nil unless WithHistoryRepository was given. It drives
	// runtime History persistence for this agent (ADR-0017).
	historyRepo gollem.HistoryRepository
}

// BindStrategy erases the type of a Strategy by folding Init/Step/EncodeState/
// DecodeState/EncodeOutput into closures, plus the completion handler when one
// was registered. Exported for building fake strategies in tests.
func BindStrategy[S, I, O any](s Strategy[S, I, O], opts ...RegisterOption[O]) StrategyBinding {
	var cfg registerConfig[O]
	for _, o := range opts {
		o(&cfg)
	}
	b := StrategyBinding{
		version: s.Version(),
		init: func(in any) (any, error) {
			typed, ok := in.(I)
			if !ok {
				return nil, goerr.Wrap(ErrInvalidRequest, "spawn input type mismatch")
			}
			return s.Init(typed)
		},
		// step, encode and encodeOutput assert with comma-ok rather than bare: a
		// Step middleware can replace the state or the Decision with a value of
		// another type, so this is reachable and must be an error a caller can
		// discriminate, not a panic reported as "strategy panic".
		step: func(ctx context.Context, sys Syscalls, st any) (any, decision, error) {
			typed, ok := st.(S)
			if !ok {
				return nil, decision{}, goerr.Wrap(ErrInvalidRequest, "step state type mismatch")
			}
			state, d, err := s.Step(ctx, sys, typed)
			if err != nil {
				return state, decision{}, err
			}
			return state, d.erase(), nil
		},
		encode: func(st any) ([]byte, error) {
			typed, ok := st.(S)
			if !ok {
				return nil, goerr.Wrap(ErrInvalidRequest, "encode state type mismatch")
			}
			return s.EncodeState(typed)
		},
		encodeOutput: func(out any) ([]byte, error) {
			env, ok := out.(typedOutput[O])
			if !ok {
				return nil, goerr.Wrap(ErrInvalidRequest, "step output type mismatch")
			}
			return s.EncodeOutput(env.value)
		},
		decode: func(v int, raw []byte) (any, error) { return s.DecodeState(v, raw) },
	}
	if cfg.onFinish != nil {
		b.finish = func(ctx context.Context, pid ProcessID, status ProcessStatus, typedOut any, f *Failure) error {
			res := FinishResult[O]{Status: status}
			switch status {
			case ProcessSucceeded:
				env, ok := typedOut.(typedOutput[O])
				if !ok {
					return goerr.Wrap(ErrInvalidRequest, "finish output type mismatch",
						goerr.V("process", pid))
				}
				res.Output = &env.value
			case ProcessFailed:
				res.Failure = f
			}
			return cfg.onFinish(ctx, pid, res)
		}
	}
	b.historyRepo = cfg.historyRepo
	return b
}
