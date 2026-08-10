package agentkit

import (
	"encoding/json"
	"maps"

	"github.com/m-mizutani/goerr/v2"
)

// MetricKey names a counter the caller defined, for the counters the kernel
// cannot measure on its own — documents fetched, credits spent, rows written.
//
// It is a sealed interface: the unexported marker method metricKey() makes it
// unimplementable outside this package, so only values returned by
// DefineMetricKey can exist. A caller therefore cannot name a counter with a
// string literal at the point of use, which is the whole reason this is not a
// map[string]int64 — a typo there produces a second counter that silently
// reads zero.
//
// Identity is the NAME, not the pointer: two DefineMetricKey calls with the same
// name are equal and index the same entry of a map[MetricKey]int64. This is the
// opposite of ModelRole (id.go), and deliberately so. A counter is persisted, so
// a Repository reading a row back must be able to rebuild the key from the
// stored name; a key rebuilt under pointer identity would never match the one
// the caller holds in a package variable, and every Count would read zero.
type MetricKey interface {
	// String returns the name, which is what a Repository stores.
	String() string
	metricKey()
}

// metricKey is the sole implementation of MetricKey. It is a comparable VALUE
// type rather than a pointer: an interface value compares equal when the
// dynamic type and the value both match, which is what makes name identity work
// through the interface.
type metricKey struct{ name string }

func (k metricKey) String() string { return k.name }
func (k metricKey) metricKey()     {}

// DefineMetricKey returns the key for a counter name. Callers keep the result in
// a package variable and share that value.
//
// There is no registry behind it, so it is also how a Repository turns a stored
// name back into a key. Two packages that choose the same name get the same
// counter and their values are summed — namespace the name (myapp.docs_fetched)
// rather than expecting the kernel to detect the collision. Detecting it would
// need mutable package state and would make a name that no code in this binary
// defines — which is exactly what a Repository reads back — impossible to
// reconstruct.
//
// An empty name yields nil, so an invalid key has exactly one representation
// and every use site tests for it the same way.
func DefineMetricKey(name string) MetricKey {
	if name == "" {
		return nil
	}
	return metricKey{name: name}
}

// keyName is String() for a MetricKey that may be nil, for log lines and error
// context where a nil key is precisely what is being reported.
func keyName(k MetricKey) string {
	if k == nil {
		return ""
	}
	return k.String()
}

// Metrics is what the Kernel maintains per Process: eight counters it measures
// itself, plus any number of counters the caller defined with DefineMetricKey
// (ADR-0010).
//
// The kernel's eight are struct fields because that set is closed — a caller
// cannot add a ninth, and a map would advertise a key space that does not
// exist. The caller-defined ones are a map because that set is open by
// construction, and the key type is what keeps it safe: only a MetricKey can
// address it.
//
// Every counter is cumulative and never decreases. The zero value means
// "nothing consumed" and is a valid Metrics.
//
// Process.Metrics counts a Process's own effects plus every child that has
// terminated, once each, so a Limit high in a tree sees what the subtree
// spent rather than only the row it was called for. Caller-defined counters
// roll up the same way, which is why they are kernel-owned data summed by the
// kernel rather than something the caller accumulates itself.
//
// The caller-defined map is NEVER mutated in place: add, WithCount and
// UnmarshalJSON each build a fresh one. That is what lets a Metrics be copied by
// assignment and shared between a stored row and a value handed to a Limiter
// without a deep copy — and, since the field is unexported, there is no way for
// a caller to break it.
//
// Metrics is no longer comparable with ==, because of that map. Compare with
// reflect.DeepEqual (which is what gt does).
type Metrics struct {
	InputTokens  int64
	OutputTokens int64

	// CacheReadInputTokens and CacheCreationInputTokens are COMPONENTS OF
	// InputTokens, not additions to it — the same relation gollem.Response
	// defines, where InputToken is the true total of all three:
	// InputTokens = uncached input + CacheCreationInputTokens + CacheReadInputTokens.
	//
	// InputTokens - CacheReadInputTokens is the input NOT served from cache —
	// uncached input plus any cache write, not a single price tier: a cache
	// write is commonly billed at a premium over uncached input, and a cache
	// read at a discount. Pricing is the caller's to know (model, contract,
	// date); agentkit only carries the three counts, not a rate.
	//
	// Only Claude reports cache writes; the field is 0 for providers that do
	// not, which is indistinguishable from "caching was not used" and is
	// intentionally not corrected here.
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64

	LLMCalls  int64
	ToolCalls int64
	Steps     int64
	Spawns    int64

	// custom holds the caller-defined counters. Empty is stored as nil, never
	// as a zero-length map, so two Metrics carrying the same numbers are
	// DeepEqual whichever way each was built.
	custom map[MetricKey]int64
}

// Count reports the caller-defined counter, or 0 for a key nothing has counted
// under (and for a nil key).
func (m Metrics) Count(key MetricKey) int64 {
	if key == nil {
		return 0
	}
	return m.custom[key]
}

// WithCount returns a copy whose key counts n. It SETS rather than adds: that is
// what a Repository needs to rebuild a row it read back, and adding is the
// composition m.WithCount(k, m.Count(k)+n), which the reverse cannot express.
//
// n == 0 drops the key. A nil key is a no-op, so a Repository reading a stored
// name that is somehow empty cannot put an unaddressable entry in the map.
//
// It does not reject a negative n. The counters the kernel accumulates are
// non-negative because Syscalls.Count and the MeteredTool fold reject one
// there, where an error can be returned to whoever made the mistake.
func (m Metrics) WithCount(key MetricKey, n int64) Metrics {
	if key == nil {
		return m
	}
	if n == 0 {
		if _, ok := m.custom[key]; !ok {
			return m
		}
		if len(m.custom) == 1 {
			m.custom = nil
			return m
		}
		next := maps.Clone(m.custom)
		delete(next, key)
		m.custom = next
		return m
	}
	next := maps.Clone(m.custom)
	if next == nil {
		next = make(map[MetricKey]int64, 1)
	}
	next[key] = n
	m.custom = next
	return m
}

// Counters returns a copy of every caller-defined counter, or nil when there is
// none. It is how a Repository that does not store JSON enumerates them;
// writing to the result affects nothing.
func (m Metrics) Counters() map[MetricKey]int64 {
	if len(m.custom) == 0 {
		return nil
	}
	return maps.Clone(m.custom)
}

// add returns the element-wise sum without mutating either operand.
func (m Metrics) add(o Metrics) Metrics {
	return Metrics{
		InputTokens:              m.InputTokens + o.InputTokens,
		OutputTokens:             m.OutputTokens + o.OutputTokens,
		CacheReadInputTokens:     m.CacheReadInputTokens + o.CacheReadInputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens + o.CacheCreationInputTokens,
		LLMCalls:                 m.LLMCalls + o.LLMCalls,
		ToolCalls:                m.ToolCalls + o.ToolCalls,
		Steps:                    m.Steps + o.Steps,
		Spawns:                   m.Spawns + o.Spawns,
		custom:                   addCounters(m.custom, o.custom),
	}
}

// addCounters sums two caller-defined counter sets into a fresh map, or nil when
// both are empty.
func addCounters(a, b map[MetricKey]int64) map[MetricKey]int64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[MetricKey]int64, len(a)+len(b))
	maps.Copy(out, a)
	for k, n := range b {
		out[k] += n
	}
	return out
}

// metricsWire is the JSON form of Metrics, and the only place that form is
// declared. Metrics itself carries no struct tags: with MarshalJSON in play they
// would be dead weight that could drift from what is actually written.
//
// The eight names and their omitempty are exactly what the tags produced before
// caller-defined counters existed, so a snapshot written then still reads back.
type metricsWire struct {
	InputTokens              int64            `json:"input_tokens,omitempty"`
	OutputTokens             int64            `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int64            `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64            `json:"cache_creation_input_tokens,omitempty"`
	LLMCalls                 int64            `json:"llm_calls,omitempty"`
	ToolCalls                int64            `json:"tool_calls,omitempty"`
	Steps                    int64            `json:"steps,omitempty"`
	Spawns                   int64            `json:"spawns,omitempty"`
	Custom                   map[string]int64 `json:"custom,omitempty"`
}

// MarshalJSON writes the wire form. Metrics needs one because the caller-defined
// counters live in an unexported field — and because a map keyed by an interface
// cannot be encoded from a struct tag at all: encoding/json requires a map key
// type to be a string, an integer, or an encoding.TextMarshaler, and an
// interface type is none of those.
//
// This is kernel-owned data, not caller data: the counter names are identifiers
// the kernel sums, and Metrics has always declared its own wire names
// (ADR-0010). Caller payloads remain []byte the kernel never decodes (ADR-0007).
func (m Metrics) MarshalJSON() ([]byte, error) {
	w := metricsWire{
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheReadInputTokens:     m.CacheReadInputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens,
		LLMCalls:                 m.LLMCalls,
		ToolCalls:                m.ToolCalls,
		Steps:                    m.Steps,
		Spawns:                   m.Spawns,
	}
	if len(m.custom) > 0 {
		w.Custom = make(map[string]int64, len(m.custom))
		for k, n := range m.custom {
			w.Custom[k.String()] = n
		}
	}
	b, err := json.Marshal(w)
	if err != nil {
		return nil, goerr.Wrap(err, "marshal metrics")
	}
	return b, nil
}

// UnmarshalJSON reads the wire form, including a null (a Process that never ran
// an effect wrote one before Metrics was a struct) and a key outside the set,
// which is dropped rather than failing the load.
func (m *Metrics) UnmarshalJSON(data []byte) error {
	var w metricsWire
	if err := json.Unmarshal(data, &w); err != nil {
		return goerr.Wrap(err, "unmarshal metrics")
	}
	out := Metrics{
		InputTokens:              w.InputTokens,
		OutputTokens:             w.OutputTokens,
		CacheReadInputTokens:     w.CacheReadInputTokens,
		CacheCreationInputTokens: w.CacheCreationInputTokens,
		LLMCalls:                 w.LLMCalls,
		ToolCalls:                w.ToolCalls,
		Steps:                    w.Steps,
		Spawns:                   w.Spawns,
	}
	for name, n := range w.Custom {
		// An empty name cannot be addressed by any key a caller can hold, so
		// carrying it forward would only produce an entry nothing can read.
		out = out.WithCount(DefineMetricKey(name), n)
	}
	*m = out
	return nil
}
