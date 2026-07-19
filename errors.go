package agentkit

import "github.com/m-mizutani/goerr/v2"

// Sentinel errors. Callers discriminate with errors.Is. Handlers wrap these
// with goerr.Wrap to add context.
var (
	// ErrInvalidConfig is returned by New when a required dependency is nil.
	ErrInvalidConfig = goerr.New("invalid config")
	// ErrInvalidRequest is returned for a malformed request (wrong await kind,
	// nil payload, unknown pending child, spawn input type mismatch, ...).
	ErrInvalidRequest = goerr.New("invalid request")
	// ErrInvalidAgentDef is returned by Register for a bad agent definition
	// (empty name, version < 1, duplicate name, nil strategy).
	ErrInvalidAgentDef = goerr.New("invalid agent definition")
	// ErrUnknownAgent is returned by Spawn/SpawnChild for an unregistered agent.
	ErrUnknownAgent = goerr.New("unknown agent")
	// ErrSubjectBusy is returned by Spawn when an open Process holds the subject.
	ErrSubjectBusy = goerr.New("subject busy")
	// ErrProcessNotFound is returned when a Process does not exist.
	ErrProcessNotFound = goerr.New("process not found")
	// ErrProcessFinished is returned when acting on a terminal Process.
	ErrProcessFinished = goerr.New("process finished")
	// ErrAwaitNotFound is returned when an await row does not exist.
	ErrAwaitNotFound = goerr.New("await not found")
	// ErrAwaitClosed is returned when responding to an await that is not open
	// (first-writer-wins; the second Respond always gets this).
	ErrAwaitClosed = goerr.New("await closed")
	// ErrToolNotFound is returned by CallTool for an unknown tool name.
	ErrToolNotFound = goerr.New("tool not found")
	// ErrLimitExceeded is returned to a strategy when a Limiter stops execution
	// before an effect runs.
	ErrLimitExceeded = goerr.New("limit exceeded")
	// ErrConflict is returned by Repository.Apply when a precondition (Rev CAS,
	// Guard, or uniqueness) is not met; nothing is written.
	ErrConflict = goerr.New("conflict")
	// ErrSuspendWithoutAwait is a transition error: a Suspend produced no open
	// await and no WaitChildren elision (prevents a permanent hang).
	ErrSuspendWithoutAwait = goerr.New("suspend without await")
	// ErrRepositoryIndeterminate is returned by the filesystem reference
	// implementation after a post-rename I/O failure leaves persisted state and
	// in-memory state diverged; the Repository is fail-stopped until reopened.
	ErrRepositoryIndeterminate = goerr.New("repository indeterminate")
)
