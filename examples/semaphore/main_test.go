// The tests for the examples live in `package main` rather than the black-box
// `package main_test` this repository uses elsewhere: a main package cannot be
// imported, so a black-box test of one is not expressible in Go.
package main

import (
	"bytes"
	"context"
	"testing"
	"time"

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

// testWork is how long each Step takes in these runs. The stub answers
// instantly, so without a window the measurement below has nothing to observe:
// Steps the runtime was willing to run at once would still finish one after
// another. Long enough that two documents reliably overlap, short enough to keep
// the suite quick.
const testWork = 50 * time.Millisecond

func TestRunSerializesPerDocument(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, 2, 3, testWork))

	got := out.String()
	gt.String(t, got).Contains("spawned 6 processes across 2 documents, 3 each")
	gt.String(t, got).Contains("all 6 processes succeeded")

	// The claim of the example: one slot per document means one Step at a time
	// for that document, however many Processes are queued behind it.
	gt.String(t, got).Contains("doc-a  1   (slots 1)")
	gt.String(t, got).Contains("doc-b  1   (slots 1)")

	// And that the limit is per (key, value): with two documents in flight the
	// kernel really did run two Steps at once. A 1 here would mean nothing was
	// running in parallel at all, which would make the line above vacuous.
	gt.String(t, got).Contains("peak concurrent Step overall  2")

	// Every slot is given back by termination; nothing leaks.
	gt.String(t, got).Contains("doc-a  held=0 waiting=0 slots=0")
	gt.String(t, got).Contains("doc-b  held=0 waiting=0 slots=0")
}

// One document, more Processes: still one at a time, and all of them finish.
func TestRunSingleDocumentStaysSerial(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, 1, 4, testWork))

	got := out.String()
	gt.String(t, got).Contains("doc-a  1   (slots 1)")
	gt.String(t, got).Contains("peak concurrent Step overall  1")
	gt.String(t, got).Contains("all 4 processes succeeded")
}

func TestRunRejectsNonPositiveCounts(t *testing.T) {
	offline(t)
	var out bytes.Buffer
	gt.Error(t, run(context.Background(), &out, 0, 3, testWork))
	gt.Error(t, run(context.Background(), &out, 2, 0, testWork))
}
