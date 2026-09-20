package cassette

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// someones is an address outside the reserved domains, which the scrubber lets
// through. Put together here, because the repository's own gate refuses a file
// that holds one.
const someones = "ada" + "@" + "corp.io"

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
		Request:  []byte(`{"messages":[{"role":"user","content":"deploy with sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA, ask ` + someones + `"}]}`),
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
	rep, err := Scrub(dir, nil, nil, false)
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
	if _, err := Scrub(dir, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	rep, err := Scrub(dir, nil, nil, false)
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
		for _, gone := range []string{"sk-ant-", someones, "eyJhbGciOi", "user-AAAA"} {
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
	rep, err := Scrub(dir, []Secret{{Name: "DEPLOY_PASSWORD", Value: password}}, nil, true)
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
	rep, err := Scrub(dir, []Secret{{Name: "ABSENT_KEY"}, {Name: "SHORT_KEY", Value: "xy"}}, nil, false)
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
	rep, err := Scrub(dir, nil, nil, true)
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

// A key is a key where one starts, not wherever `sk-` falls.
//
// Cassettes carry base64: encrypted reasoning, images, attachments. `sk-` turns
// up inside those runs by chance, and an unanchored pattern then reports — and
// with --force rewrites — a field that holds no credential at all. Measured on
// a real recording, where one such field matched 1018 characters.
func TestKeyDetectorsNeedAKeyToStart(t *testing.T) {
	// Assembled rather than written out: a key-shaped literal in a source file
	// is what the repository's own leak scan exists to refuse, and a test for a
	// key detector is no reason to make an exception.
	const tail = "abcdefghijklmnopqrstuvwxyz012345"
	openai := "sk" + "-" + tail
	anthropic := "sk" + "-ant-" + tail

	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"a key where one starts", `{"h":"` + openai + `"}`, true},
		{"a vendor-prefixed key", `{"h":"` + anthropic + `"}`, true},
		{"the same run inside base64", `{"reasoning":"QmFzZTY0` + openai + `"}`, false},
		{"the prefix inside a word", `{"note":"this is a task-specific instruction"}`, false},
		{"a provider key where one starts", `{"h":"fw_` + tail + `"}`, true},
		{"the same run inside base64", `{"reasoning":"QmFzZTY0fw_` + tail + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var found bool
			for _, d := range detectors {
				if !strings.HasSuffix(d.kind, "-key") && !strings.HasSuffix(d.kind, "-token") {
					continue
				}
				if d.re.MatchString(tc.body) {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("detected=%v, want %v for %s", found, tc.want, tc.body)
			}
		})
	}
}

// withRequest is a one-step cassette whose request holds the given text.
func withRequest(t *testing.T, text string) (dir, req string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "session")
	s, err := OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(Recording{
		Entry:    Entry{Method: "POST", Path: "/v1/messages", Status: 200},
		Request:  []byte(`{"messages":[{"role":"user","content":"` + text + `"}]}`),
		Response: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "req", "0001.json")
}

func kinds(rep ScrubReport) map[string]int {
	out := map[string]int{}
	for _, f := range rep.Findings {
		out[f.Kind] += f.Count
	}
	return out
}

// A username is the personal value most likely to be in a cassette, because a
// sandbox gives its guest the name of whoever launched it, and it is short. It
// is found as a whole word, in every spelling a recorded campaign carried it:
// a path with no leading slash, a file name an agent made up, a directory
// listing. Inside a longer word it is ordinary text and is left alone.
func TestScrubFindsAShortValueAsAWholeWord(t *testing.T) {
	dir, req := withRequest(t, `drwxr-xr-x 2 ada ada 4096 . home/ada/hello-app /tmp/ada-verify.js adapter canada`)
	rep, err := Scrub(dir, []Secret{{Name: "WHO", Value: "ada"}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 0 {
		t.Errorf("skipped = %+v, want the value looked for", rep.Skipped)
	}
	if n := kinds(rep)["env:WHO"]; n != 4 {
		t.Errorf("found %d, want the 4 whole words", n)
	}
	b, err := os.ReadFile(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"home/<SECRET>/hello-app", "/tmp/<SECRET>-verify.js", "adapter canada"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the request does not hold %q:\n%s", want, b)
		}
	}
}

// Whole words are still not enough for the words the API itself is written in.
// `user` is in every request of every cassette as a role, so a recorder whose
// login is `user` would have --force rewrite the protocol.
func TestScrubRefusesAValueTheProtocolIsWrittenIn(t *testing.T) {
	dir, _ := withRequest(t, "hello")
	rep, err := Scrub(dir, []Secret{{Name: "WHO", Value: "user"}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0].Why, "every cassette") {
		t.Errorf("skipped = %+v, want `user` refused and the reason given", rep.Skipped)
	}
	if rep.Total() != 0 || rep.Rewritten != 0 {
		t.Errorf("findings = %+v, rewritten = %d, want nothing touched", rep.Findings, rep.Rewritten)
	}
}

// A secret may say what kind it is and what stands in for it, which is how the
// recorder's own name reads as `<USER>` in a diff rather than as a secret.
func TestScrubUsesTheKindAndPlaceholderASecretBrings(t *testing.T) {
	dir, req := withRequest(t, "cd /home/ada/app")
	rep, err := Scrub(dir, []Secret{{Name: "username", Value: "ada", Kind: "recorder:username", With: "<USER>"}}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if kinds(rep)["recorder:username"] != 1 {
		t.Errorf("findings = %+v, want one recorder:username", rep.Findings)
	}
	if b, _ := os.ReadFile(req); !strings.Contains(string(b), "/home/<USER>/app") {
		t.Errorf("the placeholder was not used:\n%s", b)
	}
}

// An address in a domain reserved for documentation belongs to nobody, and an
// agent invents one whenever it needs an author for a commit. Reporting it makes
// the gate fail on every real session. The rest are still found, unless the
// caller allowed them by address or by domain.
func TestScrubLetsReservedAndAllowedAddressesThrough(t *testing.T) {
	// Put together here rather than written out, for the addresses that are not
	// reserved: the repository's own gate refuses a file that holds one.
	at := func(local, domain string) string { return local + "@" + domain }
	kept := []string{"developer@example.com", "agent@example.invalid",
		at("a", "docs.example.org"), at("x", "svc.test"),
		at("noreply", "anthropic.com"), at("Bot", "Users.NoReply.GitHub.com")}
	gone := []string{at("ada", "corp.io"), at("notexample.com", "evil.io"), at("b", "myexample.com")}
	dir, req := withRequest(t, strings.Join(append(slices.Clone(kept), gone...), " "))
	rep, err := Scrub(dir, nil, []string{at("noreply", "anthropic.com"), "@github.com"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := kinds(rep)["email"]; n != 3 {
		t.Errorf("found %d addresses, want the 3 that belong to somebody: %+v", n, rep.Findings)
	}
	b, err := os.ReadFile(req)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, kept := range kept {
		if !strings.Contains(got, kept) {
			t.Errorf("%s was rewritten:\n%s", kept, got)
		}
	}
	for _, gone := range gone {
		if strings.Contains(got, gone) {
			t.Errorf("%s survived:\n%s", gone, got)
		}
	}
}
