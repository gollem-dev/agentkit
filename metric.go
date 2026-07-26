package agentkit

// Metrics is the fixed set of counters the Kernel maintains. The set is closed
// (ADR-0010): a caller cannot add one, which is why this is a struct rather
// than a map — a map would advertise a key space that does not exist.
//
// Every field is cumulative and never decreases. The zero value means "nothing
// consumed" and is a valid Metrics.
//
// Process.Metrics counts a Process's own effects plus every child that has
// terminated, once each, so a Limit high in a tree sees what the subtree
// spent rather than only the row it was called for.
//
// The json tags match the keys the previous map form produced, so a snapshot
// written before this became a struct still reads back: known counters keep
// their values, and a null (a nil map) becomes the zero value. No migration is
// needed for that.
//
// The wire form is not byte-identical, though. Zero metrics used to marshal as
// null and now marshal as {}, and a key outside this set — which the old map
// type could hold, even though the kernel never wrote one — is dropped on read
// and gone on the next write.
type Metrics struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	LLMCalls     int64 `json:"llm_calls,omitempty"`
	ToolCalls    int64 `json:"tool_calls,omitempty"`
	Steps        int64 `json:"steps,omitempty"`
	Spawns       int64 `json:"spawns,omitempty"`
}

// add returns the element-wise sum without mutating either operand.
func (m Metrics) add(o Metrics) Metrics {
	return Metrics{
		InputTokens:  m.InputTokens + o.InputTokens,
		OutputTokens: m.OutputTokens + o.OutputTokens,
		LLMCalls:     m.LLMCalls + o.LLMCalls,
		ToolCalls:    m.ToolCalls + o.ToolCalls,
		Steps:        m.Steps + o.Steps,
		Spawns:       m.Spawns + o.Spawns,
	}
}
