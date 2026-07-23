package memory_test

import (
	"testing"

	"github.com/gollem-dev/agentkit/historystore/historytest"
	"github.com/gollem-dev/agentkit/historystore/memory"
	"github.com/gollem-dev/gollem"
)

func TestConformance(t *testing.T) {
	historytest.Run(t, func(t *testing.T) gollem.HistoryRepository {
		return memory.New()
	})
}
