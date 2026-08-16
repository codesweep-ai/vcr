package cassette

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cassette with something in it that must not be published: a key quoted in a
// prompt, an address the agent was told, and a token in a recorded answer.
func scrubbable(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "leaky")
	s, err := OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(Recording{
		Entry:    Entry{Method: "POST", Path: "/v1/messages", Status: 200},
		Request:  []byte(`{"messages":[{"role":"user","content":"deploy with sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA, ask ada@example.com"}]}`),
		Response: []byte(`{"safety_identifier":"user-AAAAAAAAAAAAAAAAAAAA","text":"use Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.not-a-real-signature"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Reporting is the default, and it changes nothing. A scrub takes values out of
// a request, which changes what replay matches on, so whoever runs it sees the
// list before the files move.
func TestScrubReportsWithoutChangingAnything(t *testing.T) {
	dir := scrubbable(t)
	before, err := os.ReadFile(filepath.Join(dir, "req", "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Scrub(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total() == 0 {
		t.Fatal("a key, an address and a token were all missed")
	}
	if rep.Rewritten != 0 {
		t.Errorf("rewrote %d file(s) while only reporting", rep.Rewritten)
	}
	after, err := os.ReadFile(filepath.Join(dir, "req", "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the request was changed by a report")
	}
	kinds := map[string]bool{}
	for _, f := range rep.Findings {
		kinds[f.Kind] = true
	}
	for _, want := range []string{"anthropic-key", "email", "jwt", "account-id"} {
		if !kinds[want] {
			t.Errorf("%s was not found: %+v", want, rep.Findings)
		}
	}
}

// And with --force the values are gone, from the response as well as the
// request: a cassette is committed whole.
func TestScrubRemovesWhatItFinds(t *testing.T) {
	dir := scrubbable(t)
	if _, err := Scrub(dir, nil, true); err != nil {
		t.Fatal(err)
	}
	rep, err := Scrub(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total() != 0 {
		t.Errorf("a second pass still finds %d value(s): %+v", rep.Total(), rep.Findings)
	}
	for _, f := range []string{"req/0001.json", "resp/0001.json"} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		for _, gone := range []string{"sk-ant-", "ada@example.com", "eyJhbGciOi", "user-AAAA"} {
			if strings.Contains(string(b), gone) {
				t.Errorf("%s still holds %q:\n%s", f, gone, b)
			}
		}
	}
	// The prompt around the values survives, or the scrub has eaten the session
	// rather than the secret.
	b, err := os.ReadFile(filepath.Join(dir, "req", "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "deploy with") {
		t.Errorf("the prompt did not survive the scrub:\n%s", b)
	}
}

// A value the caller names is matched literally, so a secret with no recognized
// shape is still found — and it is reported under the variable's name rather
// than under whichever pattern happened to catch it.
func TestScrubFindsAValueNamedByTheCaller(t *testing.T) {
	dir := scrubbable(t)
	const password = "correct-horse-battery-staple-not-a-real-one"
	req := filepath.Join(dir, "req", "0001.json")
	b, err := os.ReadFile(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req, append(b, []byte(password)...), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Scrub(dir, []Secret{{Name: "DEPLOY_PASSWORD", Value: password}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, f := range rep.Findings {
		if f.Kind == "env:DEPLOY_PASSWORD" {
			named = true
		}
	}
	if !named {
		t.Errorf("the named secret was not reported as one: %+v", rep.Findings)
	}
	after, err := os.ReadFile(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), password) {
		t.Errorf("the named secret survived:\n%s", after)
	}
}

// A name that is unset, or a value short enough to match ordinary prose, is
// reported rather than dropped. A caller who asked for a variable to be looked
// for has to learn that it was not — the alternative is a cassette that reads as
// scrubbed and is not.
func TestScrubSaysWhichSecretsItCouldNotLookFor(t *testing.T) {
	dir := scrubbable(t)
	rep, err := Scrub(dir, []Secret{{Name: "ABSENT_KEY"}, {Name: "SHORT_KEY", Value: "xy"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want both reported", rep.Skipped)
	}
	if !strings.Contains(rep.Skipped[0].Why, "not set") {
		t.Errorf("an unset variable was reported as %q", rep.Skipped[0].Why)
	}
	if !strings.Contains(rep.Skipped[1].Why, "characters") {
		t.Errorf("a short value was reported as %q", rep.Skipped[1].Why)
	}
}

// The negative half: a cassette that carries nothing sensitive is left exactly
// as it was. A scrubber that rewrites ordinary prompt text is one nobody can
// afford to run before committing.
func TestScrubLeavesAnOrdinarySessionAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clean")
	s, err := OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	const prompt = `{"messages":[{"role":"user","content":"add a /version endpoint, then run make check"}]}`
	if _, err := s.Append(Recording{
		Entry:    Entry{Method: "POST", Path: "/v1/messages", Status: 200},
		Request:  []byte(prompt),
		Response: []byte(`{"text":"done: bin/cs-vcr rebuilt, 42 tests pass"}`),
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := Scrub(dir, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total() != 0 {
		t.Errorf("an ordinary session was reported as leaking: %+v", rep.Findings)
	}
	b, err := os.ReadFile(filepath.Join(dir, "req", "0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != prompt {
		t.Errorf("the request was rewritten:\n%s", b)
	}
}
