package agentkit_test

import (
	"context"
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

func TestRegisterValidation(t *testing.T) {
	strat := &scriptStrategy{step: doneStep()}

	t.Run("empty name", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		_, err := agentkit.Register(reg, "", 1, strat)
		gt.Error(t, err).Is(agentkit.ErrInvalidAgentDef)
	})
	t.Run("version < 1", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		_, err := agentkit.Register(reg, "a", 0, strat)
		gt.Error(t, err).Is(agentkit.ErrInvalidAgentDef)
	})
	t.Run("nil strategy", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		_, err := agentkit.Register[scriptState, scriptInput, []byte](reg, "a", 1, nil)
		gt.Error(t, err).Is(agentkit.ErrInvalidAgentDef)
	})
	t.Run("duplicate name", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		_, err := agentkit.Register(reg, "a", 1, strat)
		gt.NoError(t, err)
		_, err = agentkit.Register(reg, "a", 2, strat)
		gt.Error(t, err).Is(agentkit.ErrInvalidAgentDef)
	})
	t.Run("valid returns a handle carrying the agent name", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		ag, err := agentkit.Register(reg, "assistant", 1, strat)
		gt.NoError(t, err)
		gt.Value(t, ag.Name()).Equal(agentkit.AgentName("assistant"))
	})
}

func TestRegisterWithOnFinish(t *testing.T) {
	t.Run("a nil handler is rejected", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		_, err := agentkit.Register(reg, "a", 1, &scriptStrategy{step: doneStep()},
			agentkit.WithOnFinish[[]byte](nil))
		gt.Error(t, err).Is(agentkit.ErrInvalidAgentDef)
	})

	t.Run("a handler is accepted and the handle is still Agent[I]", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		ag, err := agentkit.Register(reg, "b", 1, &scriptStrategy{step: doneStep()},
			agentkit.WithOnFinish(func(context.Context, agentkit.ProcessID, agentkit.FinishResult[[]byte]) error {
				return nil
			}))
		gt.NoError(t, err)
		gt.Value(t, ag.Name()).Equal(agentkit.AgentName("b"))
	})

	t.Run("omitting the option still registers", func(t *testing.T) {
		reg := agentkit.NewRegistry()
		_, err := agentkit.Register(reg, "c", 1, &scriptStrategy{step: doneStep()})
		gt.NoError(t, err)
	})
}
