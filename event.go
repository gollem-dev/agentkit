package agentkit

import "time"

// EventType is the type of an observable Process event. Strategies may emit
// arbitrary types via sys.Emit (names that avoid the reserved three are
// recommended, not enforced).
type EventType string

const (
	EventProcessCreated  EventType = "process.created"
	EventProcessFinished EventType = "process.finished" // succeeded / failed / cancelled.
	EventAwaitCreated    EventType = "await.created"    // question only (timer/children are internal).
)

// Event is an append-only record of an observable Process occurrence. Channel
// delivery (Slack, etc.) is done by the caller subscribing to these; this
// package only provides per-Process reads.
type Event struct {
	ProcessID ProcessID
	Type      EventType
	Key       AwaitKey // target key for await.created (typed; the kernel builds no payload).
	Payload   []byte   // the sys.Emit payload verbatim (nil for kernel-emitted events).
	At        time.Time
}
