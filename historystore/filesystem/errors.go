package filesystem

import "github.com/m-mizutani/goerr/v2"

// ErrInvalidPathComponent is returned when a ProcessID or HistoryRef is unsafe
// to use as a path element (empty, or containing a path separator or "..").
var ErrInvalidPathComponent = goerr.New("invalid path component")
