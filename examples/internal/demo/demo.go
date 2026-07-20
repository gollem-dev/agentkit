// Package demo holds the small amount of wiring the agentkit examples share:
// choosing between a live Vertex AI model and a scripted stub, and waiting for
// a Process to reach a state.
//
// Everything else — building the registry, constructing the kernel, spawning
// and serving — stays visible in each example's main.go, because those calls
// are what the examples exist to show.
package demo

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/gollem-dev/gollem/llm/gemini"
	"github.com/gollem-dev/gollem/mock"
	"github.com/m-mizutani/goerr/v2"
)

const (
	// ProjectEnv and LocationEnv select the live Vertex AI model. Both must be
	// set to go live. The credentials themselves are never read from the
	// environment: gollem's Gemini client is Vertex-only and authenticates
	// through Application Default Credentials.
	ProjectEnv  = "GEMINI_PROJECT_ID"
	LocationEnv = "GEMINI_LOCATION"
)

// Token counts reported by the stub. Fixed so the metrics an example prints are
// reproducible, and non-zero so a Limiter has something to measure.
const (
	stubInputTokens  = 64
	stubOutputTokens = 16
)

// pollInterval is how often WaitProcess re-reads the Process.
const pollInterval = 50 * time.Millisecond

var (
	// ErrWaitTimeout is returned by WaitProcess when the predicate never held.
	ErrWaitTimeout = goerr.New("wait timeout")

	// ErrPartialVertexConfig is returned when exactly one of ProjectEnv and
	// LocationEnv is set. Falling back to the stub there would hide a
	// misconfiguration behind output that looks like a successful live run.
	ErrPartialVertexConfig = goerr.New("incomplete Vertex AI configuration")

	// ErrNoTurns is returned when an example asks for the stub without a script.
	ErrNoTurns = goerr.New("stub mode requires at least one turn")
)

// Turn is one scripted LLM reply, used when the examples run offline.
type Turn struct {
	Texts         []string
	FunctionCalls []*gollem.FunctionCall
}

// NewLLM returns a live Gemini client when both ProjectEnv and LocationEnv hold
// non-empty values, and a stub replaying turns otherwise. The second result
// reports which one the caller got, so an example can say so in its output.
//
// An empty value counts as unset, which is what lets a test force stub mode
// with t.Setenv(ProjectEnv, "") regardless of the developer's environment.
func NewLLM(ctx context.Context, turns ...Turn) (gollem.LLMClient, bool, error) {
	project := os.Getenv(ProjectEnv)
	location := os.Getenv(LocationEnv)

	switch {
	case project != "" && location != "":
		client, err := gemini.New(ctx, project, location)
		if err != nil {
			return nil, false, goerr.Wrap(err, "new gemini client",
				goerr.V("project", project), goerr.V("location", location))
		}
		return client, true, nil

	case project != "" || location != "":
		return nil, false, goerr.Wrap(ErrPartialVertexConfig, "set both or neither",
			goerr.V("env", []string{ProjectEnv, LocationEnv}),
			goerr.V("hasProject", project != ""), goerr.V("hasLocation", location != ""))
	}

	if len(turns) == 0 {
		return nil, false, ErrNoTurns
	}
	return stubClient(turns), false, nil
}

// ModelLabel describes which model NewLLM handed back, for an example to print.
func ModelLabel(live bool) string {
	if live {
		return "live Gemini (Vertex AI)"
	}
	return "scripted stub (set " + ProjectEnv + " and " + LocationEnv + " to run against Vertex AI)"
}

// stubClient replays turns in order and repeats the last one once the script is
// exhausted, so a strategy that loops more than the script anticipated still
// terminates instead of panicking on an index.
func stubClient(turns []Turn) gollem.LLMClient {
	var mu sync.Mutex
	idx := 0

	return &mock.LLMClientMock{
		NewSessionFunc: func(_ context.Context, _ ...gollem.SessionOption) (gollem.Session, error) {
			return &mock.SessionMock{
				GenerateFunc: func(_ context.Context, _ []gollem.Input, _ ...gollem.GenerateOption) (*gollem.Response, error) {
					mu.Lock()
					defer mu.Unlock()
					turn := turns[idx]
					if idx < len(turns)-1 {
						idx++
					}
					return &gollem.Response{
						Texts:         turn.Texts,
						FunctionCalls: turn.FunctionCalls,
						InputToken:    stubInputTokens,
						OutputToken:   stubOutputTokens,
					}, nil
				},
				// Syscalls.Generate always reads the session history to fold it
				// into the strategy's checkpoint, so this cannot be omitted.
				HistoryFunc: func() (*gollem.History, error) {
					return &gollem.History{LLType: gollem.LLMTypeGemini, Version: gollem.HistoryVersion}, nil
				},
			}, nil
		},
	}
}

// WaitProcess polls the Process until want reports true, and gives up with
// ErrWaitTimeout after timeout.
func WaitProcess(ctx context.Context, k *agentkit.Kernel, pid agentkit.ProcessID, want func(*agentkit.Process) bool, timeout time.Duration) (*agentkit.Process, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		proc, err := k.GetProcess(ctx, pid)
		if err != nil {
			return nil, goerr.Wrap(err, "get process", goerr.V("pid", pid))
		}
		if want(proc) {
			return proc, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, goerr.Wrap(ErrWaitTimeout, "process did not reach the expected state",
				goerr.V("pid", pid), goerr.V("status", proc.Status), goerr.V("timeout", timeout))
		case <-ticker.C:
		}
	}
}

// Terminal matches a Process that reached a final status.
func Terminal(p *agentkit.Process) bool { return p.Status.Terminal() }

// Waiting matches a Process parked on an await.
func Waiting(p *agentkit.Process) bool { return p.Status == agentkit.ProcessWaiting }
