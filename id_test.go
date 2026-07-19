package agentkit_test

import (
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

func TestDefineModelRole(t *testing.T) {
	t.Run("distinct identity per call even for same name", func(t *testing.T) {
		a := agentkit.DefineModelRole("planner")
		b := agentkit.DefineModelRole("planner")
		// Resolution is by pointer identity (interface == compares the pointers),
		// NOT by value: two Define calls with the same name are distinct roles.
		gt.Bool(t, a == b).False()
		gt.Bool(t, a == a).True()
	})

	t.Run("String reports the name", func(t *testing.T) {
		r := agentkit.DefineModelRole("summarizer")
		gt.Value(t, r.String()).Equal("summarizer")
	})

	t.Run("shared package-var role resolves to itself", func(t *testing.T) {
		role := agentkit.DefineModelRole("shared")
		alias := role
		gt.Bool(t, alias == role).True()
	})
}
