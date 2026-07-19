package agentkit_test

import (
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
		_, err := agentkit.Register[scriptState, scriptInput](reg, "a", 1, nil)
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
