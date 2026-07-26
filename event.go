package agentkit

import (
	"time"

	"github.com/google/uuid"
)

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
	ID        EventID // minted by the kernel; the Repository stores it verbatim.
	ProcessID ProcessID
	Type      EventType
	Key       AwaitKey // target key for await.created (typed; the kernel builds no payload).
	Payload   []byte   // the sys.Emit payload verbatim (nil for kernel-emitted events).
	At        time.Time
}

// newEvent mints an Event with a fresh uuid v7 ID. Every Event the kernel
// creates is built here rather than as a struct literal, so no construction
// site can leave the ID empty — an event without one cannot be named by a
// cursor, and a caller deduplicating on it would silently collapse two
// occurrences into one.
func newEvent(pid ProcessID, typ EventType, key AwaitKey, payload []byte, at time.Time) *Event {
	return &Event{
		ID:        EventID(uuid.Must(uuid.NewV7()).String()),
		ProcessID: pid,
		Type:      typ,
		Key:       key,
		Payload:   payload,
		At:        at,
	}
}
