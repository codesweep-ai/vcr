package cassette

import (
	"fmt"
	"strings"
	"testing"
)

// The three shapes below are the real failures this design was written from,
// trimmed to what varied. Each was a dead replay run under hash matching.

// A Codex tool-use turn: the model's call, and the shell's answer to it. The
// answer is what changes between two runs of the same session.
func codexTurn(wall, chunk, output string) string {
	return `{"model":"gpt-5.6-sol","input":[
	  {"role":"user","content":[{"type":"input_text","text":"list the files"}]},
	  {"call_id":"call_01","type":"custom_tool_call","name":"exec","input":"ls internal"},
	  {"call_id":"call_01","type":"custom_tool_call_output","output":[
	    {"type":"input_text","text":"{\"chunk_id\":\"` + chunk + `\",\"wall_time_seconds\":` + wall + `}"},
	    {"type":"input_text","text":"` + output + `"}]}]}`
}

const toolOutput = Rule("input[].output[].text")

// The failure that cost a week of ruleset bumps: the client times its own tool
// call and feeds the result back into the next request, so turn one replayed
// and turn two missed on a number that could not be the same twice.
//
// Declared volatile, it is the world answering differently — which cs-vcr never
// claimed to reproduce — and the turn aligns.
func TestAToolResultThatChangedIsToleratedWhereDeclared(t *testing.T) {
	rec := []byte(codexTurn("0.058013986", "3f9a6b4670", "cassette\\ncli\\nproxy\\n"))
	live := []byte(codexTurn("0.052383862", "c1d2b11931", "cassette\\ncli\\nproxy\\n"))

	// Undeclared, the same difference is a divergence. Nothing is tolerated by
	// default: what may vary is stated, never assumed.
	bare, err := Align(rec, live, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Matches() {
		t.Error("a difference at an undeclared path was accepted")
	}
	if len(bare.Leaf) != 1 || len(bare.Shape) != 0 {
		t.Errorf("shape=%v leaf=%v, want exactly one leaf difference", bare.Shape, bare.Leaf)
	}

	got, err := Align(rec, live, []Rule{toolOutput})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Matches() {
		t.Fatalf("the turn did not align: shape=%v leaf=%v", got.Shape, got.Leaf)
	}
	// Kept, not discarded: this is what correlation substitutes and what a run
	// reports, so a world that moved is visible rather than silent.
	if len(got.Tolerated) != 1 {
		t.Fatalf("tolerated = %v, want the one difference recorded", got.Tolerated)
	}
	if p := Generalize(got.Tolerated[0].Path); p != string(toolOutput) {
		t.Errorf("generalized path = %q, want %q — this is what calibrate emits", p, toolOutput)
	}
}

// The other half, and the one that matters more: what the agent was ASKED is
// exact. A declared tool-output path must not make a changed instruction
// tolerable, or replay would answer a question nobody asked.
func TestAChangedInstructionIsNeverTolerated(t *testing.T) {
	rec := []byte(codexTurn("0.1", "aaa", "out\\n"))
	live := []byte(strings.Replace(codexTurn("0.1", "aaa", "out\\n"),
		"list the files", "delete the auth module", 1))

	got, err := Align(rec, live, []Rule{toolOutput})
	if err != nil {
		t.Fatal(err)
	}
	if got.Matches() {
		t.Fatal("a different instruction was served the recorded answer")
	}
	if len(got.Leaf) != 1 || !strings.HasSuffix(got.Leaf[0].Path, "text") {
		t.Errorf("leaf = %v, want the instruction named", got.Leaf)
	}
}

// The model's own decision is exact too. A tool call's name and arguments come
// from the response cs-vcr just served, so they cannot drift on their own —
// and if they do, the session is not the recorded one.
func TestAChangedToolCallIsNeverTolerated(t *testing.T) {
	rec := []byte(codexTurn("0.1", "aaa", "out\\n"))
	live := []byte(strings.Replace(codexTurn("0.1", "aaa", "out\\n"), "ls internal", "rm -rf /", 1))

	got, err := Align(rec, live, []Rule{toolOutput})
	if err != nil {
		t.Fatal(err)
	}
	if got.Matches() {
		t.Fatal("a different tool call aligned with the recording")
	}
}

// Codex sent a <plugins_instructions> block in one run and not the next. The
// content array is one item shorter, and no value comparison can express that:
// the model really was asked something different, so it is a divergence
// whatever is declared volatile.
func TestAnAddedPromptBlockIsAShapeDifference(t *testing.T) {
	rec := []byte(`{"input":[{"content":[{"text":"a"},{"text":"plugins"},{"text":"b"}]}]}`)
	live := []byte(`{"input":[{"content":[{"text":"a"},{"text":"b"}]}]}`)

	got, err := Align(rec, live, []Rule{toolOutput, "input[].content[].text"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Matches() {
		t.Fatal("a prompt with a block removed aligned with the recording")
	}
	if len(got.Shape) != 1 || !strings.Contains(got.Shape[0].Why, "3 items vs 2") {
		t.Fatalf("shape = %v, want the length reported at the array", got.Shape)
	}
	// Reported at the array, not once per element: a reader needs to see that
	// an item went missing, not three unrelated text differences.
	if len(got.Leaf) != 0 {
		t.Errorf("leaf = %v, want the length difference to stand alone", got.Leaf)
	}
}

// A per-run block differs in its SHAPE as well as its values — a key present
// one run and absent the next — so declaring it volatile has to stop the
// descent rather than only excuse the leaves inside it.
func TestAVolatilePathCoversTheShapeBeneathIt(t *testing.T) {
	rec := []byte(`{"model":"m","client_metadata":{"session_id":"a","turn_id":"b"}}`)
	live := []byte(`{"model":"m","client_metadata":{"session_id":"c","window_id":"d"}}`)

	got, err := Align(rec, live, []Rule{"client_metadata"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Matches() {
		t.Fatalf("a declared per-run block was reported as a divergence: shape=%v leaf=%v", got.Shape, got.Leaf)
	}
	if len(got.Tolerated) != 1 || got.Tolerated[0].Path != "client_metadata" {
		t.Errorf("tolerated = %v, want the block itself named once", got.Tolerated)
	}
}

// Identical requests align with nothing to report, which is the ordinary case
// and has to stay silent: a run that warned about every entry would train its
// reader to ignore the warnings.
func TestIdenticalRequestsAlignSilently(t *testing.T) {
	b := []byte(codexTurn("0.1", "aaa", "out\\n"))
	got, err := Align(b, b, []Rule{toolOutput})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Matches() || len(got.Tolerated) != 0 {
		t.Errorf("identical requests reported %+v", got)
	}
}

// A value that changed type is a divergence, not a difference in value. It
// cannot be a client varying: a number where a string was is a different shape
// of request.
func TestAChangedTypeIsAShapeDifference(t *testing.T) {
	got, err := Align([]byte(`{"stream":true}`), []byte(`{"stream":"true"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shape) != 1 || !strings.Contains(got.Shape[0].Why, "boolean vs string") {
		t.Fatalf("shape = %v, want the types named", got.Shape)
	}
}

// A key on one side only is named on the side it is missing from, because that
// is the question a reader has: was it added, or did it go?
func TestAKeyOnOneSideOnlyIsNamed(t *testing.T) {
	got, err := Align([]byte(`{"a":1,"b":2}`), []byte(`{"a":1,"c":3}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shape) != 2 {
		t.Fatalf("shape = %v, want both keys reported", got.Shape)
	}
	var recordedOnly, liveOnly string
	for _, d := range got.Shape {
		if strings.Contains(d.Why, "recorded") {
			recordedOnly = d.Path
		}
		if strings.Contains(d.Why, "live") {
			liveOnly = d.Path
		}
	}
	if recordedOnly != "b" || liveOnly != "c" {
		t.Errorf("recordedOnly=%q liveOnly=%q, want b and c", recordedOnly, liveOnly)
	}
}

// The rule language is the one `strip_fields` already uses, and it has to be
// precise about what it claims: a rule covers a path at or below itself, and
// nothing beside it.
func TestRuleMatching(t *testing.T) {
	for _, c := range []struct {
		rule, path string
		want       bool
	}{
		{"input[].output[].text", "input[2].output[0].text", true},
		{"input[].output[].text", "input[0].output[11].text", true},
		{"input[].output[].text", "input[2].content[0].text", false},
		{"input[].output[].text", "input[2].output[0].other", false},
		// A rule covers everything beneath it, so a per-run block needs naming
		// once however it is shaped.
		{"client_metadata", "client_metadata.session_id", true},
		{"client_metadata", "client_metadata", true},
		{"client_metadata", "client_metadata_extra", false},
		// And never anything above it: declaring a field volatile must not
		// excuse its parent.
		{"input[].output[].text", "input[2]", false},
		{"a.b", "a", false},
		// An index where the rule names none still matches the array itself.
		{"input", "input[3]", true},
	} {
		if got := matches(c.rule, c.path); got != c.want {
			t.Errorf("matches(%q, %q) = %v, want %v", c.rule, c.path, got, c.want)
		}
	}
}

// Calibration emits rules, not observations: one difference seen at turn three
// has to become the rule that covers every turn of it.
func TestGeneralizeTurnsAnObservationIntoARule(t *testing.T) {
	for path, want := range map[string]string{
		"input[2].output[0].text":  "input[].output[].text",
		"client_metadata":          "client_metadata",
		"messages[11].content[3]":  "messages[].content[]",
		"a":                        "a",
		"input[0].content[0].text": "input[].content[].text",
	} {
		if got := Generalize(path); got != want {
			t.Errorf("Generalize(%q) = %q, want %q", path, got, want)
		}
	}
}

// Both sides come from cs-vcr's own canonicalization, so unparseable JSON means
// something upstream is broken. It must say so rather than report a mismatch
// nobody can act on.
func TestUnparseableInputIsAnErrorNotAMismatch(t *testing.T) {
	if _, err := Align([]byte(`{`), []byte(`{}`), nil); err == nil {
		t.Error("a malformed recorded request was reported as an alignment")
	}
	if _, err := Align([]byte(`{}`), []byte(`{`), nil); err == nil {
		t.Error("a malformed live request was reported as an alignment")
	}
}

// A length difference has to say WHICH item, or a reader is sent to open both
// bodies and count. Measured on a real session: the answer was a
// `<plugins_instructions>` block Codex sent on one run and not the next, and
// "4 items vs 3" did not say so.
func TestALengthDifferenceListsWhatEachSideHolds(t *testing.T) {
	rec := []byte(`{"content":[{"text":"<permissions instructions>\nFilesystem…"},` +
		`{"text":"<apps_instructions>\n## Apps…"},{"text":"<plugins_instructions>\n## Plugins…"},` +
		`{"text":"<skills_instructions>\n## Skills…"}]}`)
	live := []byte(`{"content":[{"text":"<permissions instructions>\nFilesystem…"},` +
		`{"text":"<apps_instructions>\n## Apps…"},{"text":"<skills_instructions>\n## Skills…"}]}`)

	got, err := Align(rec, live, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shape) != 1 {
		t.Fatalf("shape = %v, want one length difference", got.Shape)
	}
	why := got.Shape[0].Why
	for _, want := range []string{"4 items vs 3", "plugins_instructions", "recorded:", "this run:"} {
		if !strings.Contains(why, want) {
			t.Errorf("the report does not mention %q:\n%s", want, why)
		}
	}
	// The missing block appears on the recorded side and not on the live one,
	// which is the whole question a reader has.
	rec_, live_, _ := strings.Cut(why, "this run:")
	if !strings.Contains(rec_, "plugins_instructions") || strings.Contains(live_, "plugins_instructions") {
		t.Errorf("the sides are not distinguishable:\n%s", why)
	}
}

// A long list is not enumerated: a conversation's message array runs to
// hundreds, and printing all of them buries the difference again.
func TestALongListIsNotEnumerated(t *testing.T) {
	var a, b []string
	for i := range 20 {
		a = append(a, fmt.Sprintf(`{"text":"m%d"}`, i))
		if i < 19 {
			b = append(b, fmt.Sprintf(`{"text":"m%d"}`, i))
		}
	}
	got, err := Align([]byte(`{"messages":[`+strings.Join(a, ",")+`]}`),
		[]byte(`{"messages":[`+strings.Join(b, ",")+`]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shape) != 1 || strings.Contains(got.Shape[0].Why, "|") {
		t.Errorf("a 20-item list was enumerated: %q", got.Shape[0].Why)
	}
}

// Codex leaves a husk behind. Dropping `<plugins_instructions>` empties the
// developer message that carried it, and whether the husk is sent at all
// varies run to run — which showed up as `input: 11 items vs 10` on
// codex-subscription and stalled every replay of it. An item asking nothing
// cannot be the reason two conversations fail to correspond.
func TestAContentlessItemDoesNotBreakAlignment(t *testing.T) {
	withHusk := []byte(`{"input":[
	  {"role":"developer","content":[]},
	  {"role":"user","content":[{"type":"input_text","text":"list the files"}]}
	]}`)
	without := []byte(`{"input":[
	  {"role":"user","content":[{"type":"input_text","text":"list the files"}]}
	]}`)

	for _, tc := range []struct {
		name      string
		rec, live []byte
	}{
		{"husk recorded, absent live", withHusk, without},
		{"husk live, absent in the recording", without, withHusk},
		{"husk on both sides", withHusk, withHusk},
	} {
		got, err := Align(tc.rec, tc.live, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !got.Matches() {
			t.Errorf("%s: shape=%v leaf=%v, want a match", tc.name, got.Shape, got.Leaf)
		}
	}
}

// The tolerance is for husks and nothing else. An item with content, an item
// with no `content` key at all, and an item whose content is an empty STRING
// are each a real part of the conversation: a list that gained or lost one is
// a different question, and must still fail.
func TestOnlyAnEmptyContentListIsTolerated(t *testing.T) {
	base := `{"input":[{"role":"user","content":[{"type":"input_text","text":"go"}]}]}`
	for _, extra := range []string{
		`{"role":"user","content":[{"type":"input_text","text":"and hurry"}]}`,
		`{"type":"custom_tool_call","name":"shell"}`,
		`{"role":"developer","content":""}`,
	} {
		live := []byte(`{"input":[` + extra + `,` +
			`{"role":"user","content":[{"type":"input_text","text":"go"}]}]}`)
		got, err := Align([]byte(base), live, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Matches() {
			t.Errorf("an added %s was accepted; only a contentless husk may be", extra)
		}
	}
}
