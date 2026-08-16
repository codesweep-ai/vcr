// Command cs-vcr records and replays the HTTP traffic between AI coding agents
// and LLM providers.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/codesweep-ai/vcr/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		var status *cli.ExitStatus
		switch {
		case errors.As(err, &status):
			// A replay session hit a cassette miss. The command has already
			// reported what happened, in the form its caller parses; hand back
			// the status and say nothing more.
			os.Exit(status.Code)
		default:
			fmt.Fprintln(os.Stderr, "cs-vcr: "+err.Error())
			os.Exit(1)
		}
	}
}
