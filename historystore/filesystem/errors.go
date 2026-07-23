package filesystem

import "github.com/m-mizutani/goerr/v2"

// ErrInvalidSessionID is returned when a sessionID is unsafe to use as a
// filename (empty, or containing a path separator or "..").
var ErrInvalidSessionID = goerr.New("invalid session id")
