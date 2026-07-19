package agentkit_test

import (
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
