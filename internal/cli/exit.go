package cli

import "fmt"

// ExitStatus carries a specific exit code out of a command, for the failure a
// caller acts on rather than reads: a replay session that could not serve a
// request has to fail the build, and a CI script should be able to
// tell that from "cs-vcr could not start" without parsing text.
type ExitStatus struct {
	Code   int
	Reason string
}

func (e *ExitStatus) Error() string { return fmt.Sprintf("%s (exit %d)", e.Reason, e.Code) }

// Exit codes. 1 is left to ordinary errors, so a caller can tell "cs-vcr could
// not run" from "cs-vcr ran and the session failed".
const (
	// ExitCassetteMiss is a replay session that could not serve a request.
	ExitCassetteMiss = 4
)
