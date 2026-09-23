package agents

import (
	"regexp"
	"strings"
	"testing"
)

// versions is where replaySkip puts the two versions: last, in brackets.
var versions = regexp.MustCompile(` \[recorded [^\]]+, host [^\]]+\]$`)

// A run is compared against an earlier one by its skips, so the reason has to
// say which case it is in words that stay put while the host's agent updates.
func TestReplaySkipNamesTheCaseInWordsThatDoNotMove(t *testing.T) {
	cases := []struct {
		name, host, words, versions string
	}{
		{"absent", "", "claude is not installed on this host", "[recorded 2.1.245, host none]"},
		{"newer", "2.1.258", "claude on this host is newer than the one it was recorded with", "[recorded 2.1.245, host 2.1.258]"},
		{"older", "2.1.99", "claude on this host is older than the one it was recorded with", "[recorded 2.1.245, host 2.1.99]"},
	}
	for _, c := range cases {
		got := replaySkip("claude-code-api-key", "claude", "2.1.245", c.host)
		if !strings.HasPrefix(got, "claude-code-api-key cannot replay: "+c.words+".") {
			t.Errorf("%s: %q does not open with %q", c.name, got, c.words)
		}
		if !strings.HasSuffix(got, " "+c.versions) {
			t.Errorf("%s: %q does not end with %q", c.name, got, c.versions)
		}
		if !strings.Contains(got, "Install claude@2.1.245") {
			t.Errorf("%s: %q does not say what to install", c.name, got)
		}
	}
}

func TestReplaySkipIsTheSameReasonAcrossAgentUpdates(t *testing.T) {
	for _, pair := range [][2]string{{"2.1.258", "2.1.280"}, {"2.1.99", "2.1.200"}} {
		a := replaySkip("claude-code-api-key", "claude", "2.1.245", pair[0])
		b := replaySkip("claude-code-api-key", "claude", "2.1.245", pair[1])
		if a == b {
			t.Fatalf("host %s and host %s gave one reason, so the versions are missing: %q", pair[0], pair[1], a)
		}
		if versions.ReplaceAllString(a, "") != versions.ReplaceAllString(b, "") {
			t.Errorf("hosts %s and %s differ outside the versions:\n  %q\n  %q", pair[0], pair[1], a, b)
		}
	}
}

func TestReplaySkipIsEmptyAtTheRecordedVersion(t *testing.T) {
	if got := replaySkip("codex-api-key", "codex", "0.149.1", "0.149.1"); got != "" {
		t.Errorf("the recorded version skipped: %q", got)
	}
}

// Compared as text, 2.1.99 sorts after 2.1.245 and would read as newer.
func TestNewerVersionComparesNumbers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.1.280", "2.1.245", true},
		{"2.1.99", "2.1.245", false},
		{"0.152.1", "0.149.1", true},
		{"1.10.0", "1.9.9", true},
		{"1.9.9", "1.10.0", false},
		{"1.18.22", "1.18.22", false},
	} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
