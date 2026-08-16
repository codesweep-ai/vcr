package agents

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// workspace is where one scenario's run happens: a home for the agent's
// configuration, and a directory for it to work in.
//
// It lives outside the checkout, and outside any git repository. Both agents
// that report their surroundings say whether they are in one and what its branch
// and status are, so a workspace inside this repo would put the state of this
// repo into every prompt — and a replay in CI would then have to reproduce a
// working tree.
//
// It also lives outside the system temporary directory, for two measured
// reasons. Codex refuses to install its shell helpers under one and says so. And
// it lists the sandbox's writable roots in sorted order, so a workspace under
// /tmp reorders that sentence in the prompt and every request misses — a
// cassette recorded in a cache directory replays in any other cache directory,
// on Linux and macOS alike, because all of them sort before /tmp.
type workspace struct {
	root string
	home string
	work string
}

// newWorkspace builds a clean workspace for one scenario in one mode.
//
// Clean, not reused: the second run of an agent behaves differently from the
// first — a cached model list, a session file, an onboarding flag — and a
// fixture recorded by a warm agent does not replay against a cold one.
func newWorkspace(scenario, mode string) (*workspace, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("no cache directory to build a workspace in: %w", err)
	}
	root := filepath.Join(cache, "cs-vcr", "agent-fixtures", scenario+"-"+mode)
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	ws := &workspace{root: root, home: filepath.Join(root, "home"), work: filepath.Join(root, "work")}
	for _, d := range []string{ws.home, ws.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// write puts a file under the workspace home, creating its directory.
func (w *workspace) write(rel, content string, perm os.FileMode) error {
	p := filepath.Join(w.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), perm)
}

// copyInto copies a credential file the developer already has into the
// workspace, so the recording half authenticates as they do.
//
// A copy rather than the file itself: an agent rewrites its credential store
// when it feels like it, and a test has no business editing the login a
// developer uses for their own work.
func (w *workspace) copyInto(rel, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return w.write(rel, string(b), 0o600)
}

// env is the whole environment the agent is given.
//
// Whole, because the alternative is inheriting the developer's, and half of what
// an agent puts in its prompt comes from there: SHELL decides what it reports as
// the shell, and a stray ANTHROPIC_API_KEY decides which credential it presents.
// A CI runner has neither, so a suite that inherits records something CI cannot
// reproduce.
func (w *workspace) env(extra map[string]string) []string {
	env := map[string]string{
		// Enough PATH to find the agent, node, and the shell tools an agent
		// calls. Taken from the parent, because a developer's agent is on a
		// version-manager path that nothing else could guess.
		"PATH": os.Getenv("PATH"),
		"HOME": w.home,
		// A UTF-8 locale, fixed: an agent that reports the terminal's encoding
		// would otherwise report the developer's.
		"LANG": "C.UTF-8",
		// No terminal. Every one of these agents draws differently when it
		// thinks it has one, and one of them asks it how wide it is.
		"TERM": "dumb",
		// The agent's only route to a network is cs-vcr, on loopback. Everything
		// else fails to connect, in the recording half as well as the replay
		// half — see the package comment for what changes when it does not.
		"HTTP_PROXY":  deadProxy,
		"HTTPS_PROXY": deadProxy,
		"ALL_PROXY":   deadProxy,
		"NO_PROXY":    "127.0.0.1,localhost",
		"no_proxy":    "127.0.0.1,localhost",
	}
	maps.Copy(env, extra)
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// deadProxy is a port nothing listens on, which is how the agent is denied
// egress. A refused connection rather than a black hole: an agent that hangs
// until its own timeout turns a two-minute suite into a twenty-minute one.
const deadProxy = "http://127.0.0.1:1"

// hello is what the prompt asks for, and what both halves assert afterwards.
// Small on purpose: a fixture is a few thousand tokens to record and a diff a
// reviewer can read, and one tool call is enough to exercise the loop that
// matters — the model asks for something, the world answers, the model replies.
const (
	prompt     = "Create a file named hello.txt containing exactly: hello world. Then reply with the single word done."
	helloFile  = "hello.txt"
	helloText  = "hello world"
	doneAnswer = "done"
)

// wroteHello reports whether the agent did what it was asked, which is the
// assertion that a replayed session did the same work as the recorded one.
func (w *workspace) wroteHello() error {
	b, err := os.ReadFile(filepath.Join(w.work, helloFile))
	if err != nil {
		return fmt.Errorf("the agent did not create %s: %w", helloFile, err)
	}
	if !strings.Contains(string(b), helloText) {
		return fmt.Errorf("%s holds %q, want it to contain %q", helloFile, strings.TrimSpace(string(b)), helloText)
	}
	return nil
}
