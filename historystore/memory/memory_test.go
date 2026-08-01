package memory_test

import (
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/historystore/historytest"
	"github.com/gollem-dev/agentkit/historystore/memory"
)

func TestConformance(t *testing.T) {
	historytest.Run(t, func(t *testing.T) agentkit.HistoryStore {
		return memory.New()
	})
}
