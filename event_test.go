package agentkit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

func TestEventJSONRoundTrip(t *testing.T) {
	at := time.Unix(500, 0).UTC()
	orig := agentkit.Event{
		ProcessID: "p-1",
		Type:      agentkit.EventAwaitCreated,
		Key:       "q:1",
		Payload:   []byte(`{"text":"hi"}`),
		At:        at,
	}
	raw, err := json.Marshal(orig)
	gt.NoError(t, err)
	var got agentkit.Event
	gt.NoError(t, json.Unmarshal(raw, &got))
	gt.Value(t, got.Type).Equal(agentkit.EventAwaitCreated)
	gt.Value(t, got.Key).Equal(agentkit.AwaitKey("q:1"))
	gt.Value(t, string(got.Payload)).Equal(`{"text":"hi"}`)
	gt.Value(t, got.At.Equal(at)).Equal(true)
}

// Every event the kernel writes carries an ID, because an event without one
// cannot be named by a cursor and would collapse with its neighbours under a
// caller's deduplication.
func TestKernelEventsAllCarryDistinctIDs(t *testing.T) {
	ctx := context.Background()
	model, _ := mockLLM(textResponse("x"))

	step := func(c context.Context, sys agentkit.Syscalls, st scriptState) (scriptState, agentkit.Decision[[]byte], error) {
		if st.N == 0 {
			if e := sys.Emit(c, "test.progress", []byte(`{"n":1}`)); e != nil {
				return st, agentkit.Decision[[]byte]{}, e
			}
			st.N = 1
			return st, agentkit.Suspend[[]byte](agentkit.Question("q", []byte("ok?"))), nil
		}
		return st, agentkit.Done([]byte("done")), nil
	}
	k, repo, ag := setupScript(t, step, model)
	pid, err := ag.Spawn(ctx, k, scriptInput{Seed: "s"})
	gt.NoError(t, err)
	serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessWaiting
	})
	gt.NoError(t, k.Respond(ctx, pid, "q", []byte("yes")))
	serveUntil(t, k, repo, pid, 5*time.Second, func(p *agentkit.Process) bool {
		return p.Status == agentkit.ProcessSucceeded
	})

	events, err := k.ListEvents(ctx, pid)
	gt.NoError(t, err)

	seen := map[agentkit.EventID]bool{}
	types := map[agentkit.EventType]bool{}
	for _, e := range events {
		gt.Value(t, e.ID).NotEqual(agentkit.EventID(""))
		gt.Value(t, seen[e.ID]).Equal(false)
		seen[e.ID] = true
		types[e.Type] = true
	}
	// All four construction sites: created, await.created, sys.Emit, finished.
	gt.Value(t, types[agentkit.EventProcessCreated]).Equal(true)
	gt.Value(t, types[agentkit.EventAwaitCreated]).Equal(true)
	gt.Value(t, types[agentkit.EventProcessFinished]).Equal(true)
	gt.Value(t, types[agentkit.EventType("test.progress")]).Equal(true)
}
