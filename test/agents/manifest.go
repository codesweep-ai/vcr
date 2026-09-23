package agents

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// manifestPath is the committed record of what each fixture was recorded with.
const manifestPath = "fixtures.json"

// manifest is what the replay half checks itself against, and what CI installs.
//
// An agent's own version is in its prompt — Claude Code sends `cc_version=` in
// the first system block, and every one of them ships a new system prompt with
// each release — so a cassette is a recording of one build of one agent. Without
// this file, a contributor whose agent updated overnight gets a wall of prompt
// diff and no statement of the cause; with it, the run says which version the
// fixture holds.
type manifest struct {
	// Prompt is what every scenario was asked. Recorded so a change to it is
	// visible in the diff that re-records the fixtures.
	Prompt string `json:"prompt"`
	// Fixtures are keyed by scenario name.
	Fixtures map[string]fixture `json:"fixtures"`
}

type fixture struct {
	Agent   string `json:"agent"`
	Version string `json:"agent_version"`
	Auth    string `json:"auth"`
	Model   string `json:"model"`
	// Steps is how many requests the recorded session made. The replay half
	// asserts it served all of them: a session that replayed half its script and
	// stopped is not a session that replayed.
	Steps    int    `json:"steps"`
	Recorded string `json:"recorded"`
}

func loadManifest(path string) (*manifest, error) {
	m := &manifest{Prompt: prompt, Fixtures: map[string]fixture{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Fixtures == nil {
		m.Fixtures = map[string]fixture{}
	}
	return m, nil
}

// save writes the manifest with sorted keys and a trailing newline, so
// re-recording one scenario produces a one-block diff rather than a reordering.
func (m *manifest) save(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// names lists the recorded scenarios in a stable order.
func (m *manifest) names() []string {
	out := make([]string, 0, len(m.Fixtures))
	for n := range m.Fixtures {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// agentVersion asks an agent what it is. Every one of them answers `--version`
// with a line carrying a semantic version and nothing else in common, so the
// version is taken as the first one in the answer.
func agentVersion(bin string) (string, error) {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %v (%s)", bin, err, strings.TrimSpace(string(out)))
	}
	m := regexp.MustCompile(`\d+\.\d+\.\d+`).FindString(string(out))
	if m == "" {
		return "", fmt.Errorf("%s --version printed no version: %q", bin, strings.TrimSpace(string(out)))
	}
	return m, nil
}

// replaySkip says why a fixture cannot replay on this host, or "" when it can.
// host is the agent's version here, "" when it is not installed.
//
// The words say which case it is, absent, newer or older, and stay the same
// while the host's agent updates. Both versions go last, in brackets, so a run
// compared against an earlier one can set them aside and still see the skip.
func replaySkip(name, bin, recorded, host string) string {
	versions := fmt.Sprintf("[recorded %s, host %s]", recorded, cmp.Or(host, "none"))
	switch {
	case host == "":
		return fmt.Sprintf("%s cannot replay: %s is not installed on this host. Install %s@%s %s",
			name, bin, bin, recorded, versions)
	case host == recorded:
		return ""
	case newerVersion(host, recorded):
		return fmt.Sprintf("%s cannot replay: %s on this host is newer than the one it was recorded with. "+
			"Install %s@%s, or re-record with `make fixtures` %s", name, bin, bin, recorded, versions)
	default:
		return fmt.Sprintf("%s cannot replay: %s on this host is older than the one it was recorded with. "+
			"Install %s@%s %s", name, bin, bin, recorded, versions)
	}
}

// newerVersion compares two x.y.z versions field by field, as numbers.
func newerVersion(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x > y
		}
	}
	return len(as) > len(bs)
}
