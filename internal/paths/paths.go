// Package paths resolves where cs-vcr keeps its files, separating two concerns
// that want to live in different places:
//
//   - CONFIG (the config file, credentials) -> $XDG_CONFIG_HOME/cs-vcr.
//     Per-user, edited by hand, never written by the proxy.
//   - CASSETTES -> the working directory, NOT an XDG dir. A cassette is a
//     project asset that belongs in the repo it records: it is reviewed in a
//     PR diff and gates a merge. Putting it under a
//     per-user directory would make it invisible to exactly the audience it
//     exists for.
//
// Both have an env override; CS_VCR_HOME relocates the config under one root.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

const app = "cs-vcr"

// Config is the config file: providers, provider pins, the normalization ruleset.
//
// named reports whether someone pointed at that file outright, which is what
// makes a missing one an error rather than the ordinary case. CS_VCR_CONFIG is
// a file; CS_VCR_HOME and the XDG default are places to look, and a place with
// nothing in it is how most machines run.
func Config() (path string, named bool) {
	if p := os.Getenv("CS_VCR_CONFIG"); p != "" {
		return p, true
	}
	if h := os.Getenv("CS_VCR_HOME"); h != "" {
		return filepath.Join(h, "config.yaml"), false
	}
	return filepath.Join(configHome(), app, "config.yaml"), false
}

// The cassette store is deliberately not resolved here. config.Load owns
// CS_VCR_CASSETTES and the "cassettes" default, because the store is a config
// key as well as an environment variable and one of the two had to win. A
// second copy of that rule lived in this package and nothing reached it:
// changing the default there would have moved nothing.

func configHome() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home(), "Library", "Application Support")
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(), ".config")
}

// home is the user's home directory, falling back to "." so that a process
// with no HOME (a container built without one) still resolves to a usable
// relative path rather than to the filesystem root.
func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}
