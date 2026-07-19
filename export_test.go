package agentkit

import (
	"time"

	"github.com/gollem-dev/gollem"
)

// ResolveModel exposes resolveModel for testing role resolution.
func (k *Kernel) ResolveModel(role ModelRole) gollem.LLMClient { return k.resolveModel(role) }

// Exposed StrategyBinding closures for white-box testing of BindStrategy.
func (b StrategyBinding) InitForTest(in any) (any, error)              { return b.init(in) }
func (b StrategyBinding) EncodeForTest(st any) ([]byte, error)         { return b.encode(st) }
func (b StrategyBinding) DecodeForTest(v int, raw []byte) (any, error) { return b.decode(v, raw) }
func (b StrategyBinding) VersionForTest() int                          { return b.version }

// This file exposes internal helpers for white-box testing (package agentkit),
// consumed by the black-box test package agentkit_test.

// AddMetrics is the exported form of addMetrics for testing.
func AddMetrics(a, b Metrics) Metrics { return addMetrics(a, b) }

// CloneProcess is the exported form of (*Process).clone for testing.
func CloneProcess(p *Process) *Process { return p.clone() }

// AwaitSpecView exposes an AwaitSpec's private fields for testing.
type AwaitSpecView struct {
	Key      AwaitKey
	Kind     AwaitKind
	Payload  []byte
	Deadline *time.Time
	Children []ProcessID
}

// ViewAwaitSpec returns the private fields of an AwaitSpec.
func ViewAwaitSpec(s AwaitSpec) AwaitSpecView {
	return AwaitSpecView{
		Key:      s.key,
		Kind:     s.kind,
		Payload:  s.payload,
		Deadline: s.deadline,
		Children: s.children,
	}
}
