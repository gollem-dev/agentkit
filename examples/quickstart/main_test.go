// The tests for the examples live in `package main` rather than the black-box
// `package main_test` this repository uses elsewhere: a main package cannot be
// imported, so a black-box test of one is not expressible in Go.
package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/m-mizutani/gt"
)

// offline forces stub mode so the test never reaches Vertex AI, whatever the
// developer's environment holds.
func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

func TestRunAnswersAndReportsMetrics(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, "why does durability matter?"))

	got := out.String()
	gt.String(t, got).Contains("status:  succeeded")
	gt.String(t, got).Contains("checkpoints an agent after every step")
	gt.String(t, got).Contains("llm_calls")
}
