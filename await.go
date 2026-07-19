package agentkit

import "time"

// AwaitKind is the kind of a "wait". Human confirmation (formerly approval) is
// NOT a distinct kind — a go/no-go confirmation is just a question.
type AwaitKind string

const (
	AwaitQuestion AwaitKind = "question" // question to a human (confirmation is a yes/no question).
	AwaitTimer    AwaitKind = "timer"
	AwaitChildren AwaitKind = "children"
)

// AwaitStatus is the state of a wait.
type AwaitStatus string

const (
	AwaitOpen      AwaitStatus = "open"
	AwaitResponded AwaitStatus = "responded"
	AwaitExpired   AwaitStatus = "expired"   // deadline reached (question).
	AwaitCancelled AwaitStatus = "cancelled" // closed by the Process finishing/cancelling.
)

// Await is the persisted representation of a wait. Response acceptance upholds
// three invariants: only open is accepted, first-writer-wins, and the responder
// is recorded (optionally, via WithRespondedBy).
type Await struct {
	ProcessID ProcessID
	Key       AwaitKey
	Kind      AwaitKind
	Status    AwaitStatus
	Deadline  *time.Time

	// Kind-specific fields (only the matching kind is non-zero). The kernel puts
	// them on the row typed; row->bytes is the Repository implementation's job.
	Question []byte        // question: the Question() payload verbatim (encoding is the caller's).
	Response []byte        // response to a question (whatever Respond received; e.g. "yes"/"no").
	Children []ProcessID   // children: the children being waited on.
	Results  []ChildResult // children: the response (assembled by the kernel when all children finish).
	Fired    bool          // timer: fired.

	RespondedBy string
	CreatedAt   time.Time
	RespondedAt *time.Time
}

// ChildResult is one child's outcome, delivered to the parent's children Await.
type ChildResult struct {
	ProcessID ProcessID
	Status    ProcessStatus
	Output    []byte // the child's Output verbatim (format/type is known to the spawner).
	Failure   *Failure
}
