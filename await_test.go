package agentkit_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

// A Repository implementation must be able to persist an Await's kind-specific
// typed fields and read them back typed. This verifies the struct round-trips
// through JSON (the encoding the reference repositories use).
func TestAwaitJSONRoundTrip(t *testing.T) {
	deadline := time.Unix(1000, 0).UTC()
	respAt := time.Unix(1200, 0).UTC()
	orig := agentkit.Await{
		ProcessID: "p-1",
		Key:       "q:1",
		Kind:      agentkit.AwaitQuestion,
		Status:    agentkit.AwaitResponded,
		Deadline:  &deadline,
		Question:  []byte(`{"text":"confirm?"}`),
		Response:  []byte(`yes`),
		Children:  []agentkit.ProcessID{"c-1", "c-2"},
		Results: []agentkit.ChildResult{
			{ProcessID: "c-1", Status: agentkit.ProcessSucceeded, Output: []byte("ok")},
			{ProcessID: "c-2", Status: agentkit.ProcessFailed, Failure: &agentkit.Failure{Code: agentkit.FailureStrategyError, Message: "x"}},
		},
		Fired:       true,
		RespondedBy: "slack:U1",
		RespondedAt: &respAt,
	}

	raw := gt.R1(json.Marshal(orig)).NoError(t)
	var got agentkit.Await
	gt.NoError(t, json.Unmarshal(raw, &got))

	gt.Value(t, got.Kind).Equal(agentkit.AwaitQuestion)
	gt.Value(t, string(got.Question)).Equal(`{"text":"confirm?"}`)
	gt.Value(t, string(got.Response)).Equal("yes")
	gt.Array(t, got.Children).Length(2)
	gt.Array(t, got.Results).Length(2)
	gt.Value(t, got.Results[1].Failure.Message).Equal("x")
	gt.Value(t, got.Fired).Equal(true)
	gt.Value(t, got.Deadline.Equal(deadline)).Equal(true)
}
