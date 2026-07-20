package agentkit_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

// bindStrat is a minimal Strategy[bindState, bindInput] for exercising
// BindStrategy's type erasure directly.
type bindStrat struct{}

type bindInput struct {
	V int `json:"v"`
}

type bindState struct {
	V int `json:"v"`
}

func (bindStrat) Version() int { return 7 }
func (bindStrat) Init(in bindInput) (bindState, error) {
	return bindState{V: in.V * 2}, nil
}
func (bindStrat) Step(_ context.Context, _ agentkit.Syscalls, st bindState) (bindState, agentkit.Decision[[]byte], error) {
	return st, agentkit.Done([]byte("x")), nil
}
func (bindStrat) EncodeOutput(out []byte) ([]byte, error)  { return out, nil }
func (bindStrat) EncodeState(st bindState) ([]byte, error) { return json.Marshal(st) }
func (bindStrat) DecodeState(_ int, raw []byte) (bindState, error) {
	var st bindState
	err := json.Unmarshal(raw, &st)
	return st, err
}

func TestBindStrategy(t *testing.T) {
	b := agentkit.BindStrategy(bindStrat{})

	gt.Value(t, b.VersionForTest()).Equal(7)

	t.Run("Init type-checks and runs the typed strategy", func(t *testing.T) {
		out, err := b.InitForTest(bindInput{V: 5})
		gt.NoError(t, err)
		st, ok := out.(bindState)
		gt.Bool(t, ok).True()
		gt.Value(t, st.V).Equal(10)
	})

	t.Run("Init with wrong input type -> ErrInvalidRequest", func(t *testing.T) {
		_, err := b.InitForTest("not a bindInput")
		gt.Error(t, err).Is(agentkit.ErrInvalidRequest)
	})

	t.Run("Encode/Decode round-trips the state through bytes", func(t *testing.T) {
		raw, err := b.EncodeForTest(bindState{V: 42})
		gt.NoError(t, err)
		back, err := b.DecodeForTest(7, raw)
		gt.NoError(t, err)
		gt.Value(t, back.(bindState).V).Equal(42)
	})

	t.Run("no handler registered leaves finish nil", func(t *testing.T) {
		gt.Bool(t, b.HasFinishForTest()).False()
	})
}

// outStrat is a Strategy whose output type is a struct and whose Step is
// supplied by a closure, so a test can pick the Decision it returns.
type outStrat struct {
	step        func(bindState) (agentkit.Decision[bindOut], error)
	encodeErr   error
	encodeCalls *int
}

type bindOut struct {
	Text string `json:"text"`
}

func (outStrat) Version() int                         { return 1 }
func (outStrat) Init(in bindInput) (bindState, error) { return bindState(in), nil }
func (s outStrat) Step(_ context.Context, _ agentkit.Syscalls, st bindState) (bindState, agentkit.Decision[bindOut], error) {
	d, err := s.step(st)
	return st, d, err
}
func (s outStrat) EncodeOutput(out bindOut) ([]byte, error) {
	if s.encodeCalls != nil {
		*s.encodeCalls++
	}
	if s.encodeErr != nil {
		return nil, s.encodeErr
	}
	return json.Marshal(out)
}
func (outStrat) EncodeState(st bindState) ([]byte, error) { return json.Marshal(st) }
func (outStrat) DecodeState(_ int, raw []byte) (bindState, error) {
	var st bindState
	err := json.Unmarshal(raw, &st)
	return st, err
}

func TestBindStrategyStepEncodesOutput(t *testing.T) {
	ctx := context.Background()

	t.Run("Done encodes the output and keeps the typed value", func(t *testing.T) {
		calls := 0
		b := agentkit.BindStrategy(outStrat{
			encodeCalls: &calls,
			step: func(bindState) (agentkit.Decision[bindOut], error) {
				return agentkit.Done(bindOut{Text: "hi"}), nil
			},
		})
		_, d, err := b.StepForTest(ctx, nil, bindState{V: 1})
		gt.NoError(t, err)
		gt.Value(t, d.Kind).Equal(agentkit.DecisionDone)
		gt.Value(t, string(d.Output)).Equal(`{"text":"hi"}`)
		gt.Value(t, d.Typed).Equal(bindOut{Text: "hi"})
		gt.Value(t, calls).Equal(1)
	})

	t.Run("a non-Done decision never calls EncodeOutput", func(t *testing.T) {
		calls := 0
		b := agentkit.BindStrategy(outStrat{
			encodeCalls: &calls,
			step: func(bindState) (agentkit.Decision[bindOut], error) {
				return agentkit.Continue[bindOut](), nil
			},
		})
		_, d, err := b.StepForTest(ctx, nil, bindState{V: 1})
		gt.NoError(t, err)
		gt.Value(t, d.Kind).Equal(agentkit.DecisionContinue)
		gt.Nil(t, d.Output)
		gt.Nil(t, d.Typed)
		gt.Value(t, calls).Equal(0)
	})

	t.Run("EncodeOutput error aborts the transition", func(t *testing.T) {
		b := agentkit.BindStrategy(outStrat{
			encodeErr: gollemErr("boom"),
			step: func(bindState) (agentkit.Decision[bindOut], error) {
				return agentkit.Done(bindOut{Text: "hi"}), nil
			},
		})
		_, d, err := b.StepForTest(ctx, nil, bindState{V: 1})
		gt.Error(t, err)
		gt.Value(t, d.Kind).Equal(agentkit.DecisionKind(""))
		gt.Nil(t, d.Typed)
	})
}

func TestBindStrategyFinish(t *testing.T) {
	ctx := context.Background()
	doneStep := func(bindState) (agentkit.Decision[bindOut], error) {
		return agentkit.Done(bindOut{Text: "hi"}), nil
	}

	bindWith := func(h agentkit.FinishHandler[bindOut]) agentkit.StrategyBinding {
		return agentkit.BindStrategy(outStrat{step: doneStep}, agentkit.WithOnFinish(h))
	}

	t.Run("succeeded carries the typed output and no failure", func(t *testing.T) {
		var got agentkit.FinishResult[bindOut]
		b := bindWith(func(_ context.Context, _ agentkit.ProcessID, res agentkit.FinishResult[bindOut]) error {
			got = res
			return nil
		})
		gt.Bool(t, b.HasFinishForTest()).True()
		err := b.FinishForTest(ctx, "p-1", agentkit.ProcessSucceeded, bindOut{Text: "hi"}, nil)
		gt.NoError(t, err)
		gt.Value(t, got.Status).Equal(agentkit.ProcessSucceeded)
		gt.NotNil(t, got.Output)
		gt.Value(t, *got.Output).Equal(bindOut{Text: "hi"})
		gt.Nil(t, got.Failure)
	})

	t.Run("failed carries the failure and no output", func(t *testing.T) {
		var got agentkit.FinishResult[bindOut]
		b := bindWith(func(_ context.Context, _ agentkit.ProcessID, res agentkit.FinishResult[bindOut]) error {
			got = res
			return nil
		})
		f := &agentkit.Failure{Code: agentkit.FailureStrategyError, Message: "nope"}
		gt.NoError(t, b.FinishForTest(ctx, "p-2", agentkit.ProcessFailed, nil, f))
		gt.Value(t, got.Status).Equal(agentkit.ProcessFailed)
		gt.Nil(t, got.Output)
		gt.NotNil(t, got.Failure)
		gt.Value(t, got.Failure.Code).Equal(agentkit.FailureStrategyError)
		gt.Value(t, got.Failure.Message).Equal("nope")
	})

	t.Run("cancelled carries neither output nor failure", func(t *testing.T) {
		var got agentkit.FinishResult[bindOut]
		b := bindWith(func(_ context.Context, _ agentkit.ProcessID, res agentkit.FinishResult[bindOut]) error {
			got = res
			return nil
		})
		gt.NoError(t, b.FinishForTest(ctx, "p-3", agentkit.ProcessCancelled, nil, nil))
		gt.Value(t, got.Status).Equal(agentkit.ProcessCancelled)
		gt.Nil(t, got.Output)
		gt.Nil(t, got.Failure)
	})

	t.Run("a mismatched typed output is rejected before the handler runs", func(t *testing.T) {
		called := false
		b := bindWith(func(_ context.Context, _ agentkit.ProcessID, _ agentkit.FinishResult[bindOut]) error {
			called = true
			return nil
		})
		err := b.FinishForTest(ctx, "p-4", agentkit.ProcessSucceeded, "not a bindOut", nil)
		gt.Error(t, err).Is(agentkit.ErrInvalidRequest)
		gt.Bool(t, called).False()
	})

	t.Run("the handler's error is propagated to the caller", func(t *testing.T) {
		b := bindWith(func(_ context.Context, _ agentkit.ProcessID, _ agentkit.FinishResult[bindOut]) error {
			return gollemErr("handler failed")
		})
		gt.Error(t, b.FinishForTest(ctx, "p-5", agentkit.ProcessSucceeded, bindOut{}, nil))
	})
}
