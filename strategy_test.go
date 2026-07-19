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
func (bindStrat) Step(_ context.Context, _ agentkit.Syscalls, st bindState) (bindState, agentkit.Decision, error) {
	return st, agentkit.Done([]byte("x")), nil
}
func (bindStrat) EncodeState(st bindState) ([]byte, error) { return json.Marshal(st) }
func (bindStrat) DecodeState(_ int, raw []byte) (bindState, error) {
	var st bindState
	err := json.Unmarshal(raw, &st)
	return st, err
}

func TestBindStrategy(t *testing.T) {
	b := agentkit.BindStrategy[bindState, bindInput](bindStrat{})

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
}
