package memory_test

import (
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/repository/repotest"
)

func TestConformance(t *testing.T) {
	repotest.Run(t, func(t *testing.T) agentkit.Repository {
		return memory.New()
	})
}
