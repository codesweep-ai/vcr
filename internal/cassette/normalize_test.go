package cassette

import (
	"fmt"
	"strings"
	"testing"
)

// testRules is the identity ruleset: these tests are about canonicalization,
// which happens before any user rule is applied.
type testRules struct{}

func (testRules) StripFields() []string { return nil }

func (testRules) StripQuery() []string { return nil }

func (testRules) DropBlocks() []string { return nil }

func (testRules) Apply(b []byte) ([]byte, map[string]string) { return b, nil }

// queryRules is testRules with the default query strip list, for the cases
// about the request target rather than the body.
type queryRules struct{ testRules }

func (queryRules) StripQuery() []string { return []string{"client_version"} }

// The same message, in the two shapes the API accepts for it. A client picks
// between them on its own: Claude Code sent one message as a bare string on one
// run and as a single text block on the next, with the text identical, and a
// recorded campaign missed on the difference. No rule a user could write would
// reach it — `replace` works on bytes, and this is a change of shape.
func TestSingleTextBlockAndBareStringAreTheSameRequest(t *testing.T) {
	const text = "Available agent types for the Agent tool:"
	asString := []byte(`{"model":"m","messages":[{"role":"user","content":"` + text + `"}]}`)
	asBlock := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"` + text + `"}]}]}`)

	a := Normalize("POST", "/v1/messages", asString, testRules{})
	b := Normalize("POST", "/v1/messages", asBlock, testRules{})
	if a.Hash != b.Hash {
		t.Errorf("the two shapes hashed differently:\n--- string ---\n%s\n--- block ---\n%s", a.Canonical, b.Canonical)
	}
}

// A prompt is full of angle brackets — Codex frames its environment context in
// XML-ish tags — and escaping them for an HTML embedding nothing here does costs
// twice: the recording stops being readable in review, and a `replace` rule
// written against the prompt as the client sent it silently matches nothing.
func TestCanonicalFormKeepsAngleBracketsAsThemselves(t *testing.T) {
	body := []byte(`{"input":"<environment_context>\n  <current_date>2026-08-13</current_date>\n</environment_context>"}`)
	got := string(Normalize("POST", "/responses", body, testRules{}).Canonical)
	if !strings.Contains(got, "<current_date>2026-08-13</current_date>") {
		t.Errorf("the tag a rule would name is not in the canonical form:\n%s", got)
	}
	if strings.Contains(got, "\\u003c") {
		t.Errorf("angle brackets are still escaped:\n%s", got)
	}
}

// And it is a rendering choice, not a claim about the request: a client that
// sends the escape and one that sends the character sent the same thing, and
// must land on the same entry either way.
func TestTheTwoSpellingsOfAnAngleBracketAreOneRequest(t *testing.T) {
	plain := []byte(`{"input":"<tag>"}`)
	escaped := []byte("{\"input\":\"\\u003ctag\\u003e\"}")
	if Normalize("POST", "/responses", plain, testRules{}).Hash !=
		Normalize("POST", "/responses", escaped, testRules{}).Hash {
		t.Error("an escaped angle bracket keyed differently from the character it means")
	}
}

// The same, for the top-level system prompt, which takes both shapes too.
func TestSingleTextSystemBlockMatchesTheBareString(t *testing.T) {
	asString := []byte(`{"model":"m","system":"be brief","messages":[]}`)
	asBlock := []byte(`{"model":"m","system":[{"type":"text","text":"be brief"}],"messages":[]}`)
	if Normalize("POST", "/v1/messages", asString, testRules{}).Hash !=
		Normalize("POST", "/v1/messages", asBlock, testRules{}).Hash {
		t.Error("a one-block system prompt did not match the equivalent string")
	}
}

// Only a lone plain text block collapses. Two blocks are a different message,
// and a block carrying anything beyond type and text is left alone rather than
// guessed at.
func TestCollapseLeavesAnythingElseAlone(t *testing.T) {
	cases := map[string]string{
		"two blocks":  `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
		"extra field": `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`,
		"not text":    `{"messages":[{"role":"user","content":[{"type":"image","source":"x"}]}]}`,
	}
	for name, body := range cases {
		got := Normalize("POST", "/v1/messages", []byte(body), testRules{}).Canonical
		if !strings.Contains(string(got), `"content": [`) {
			t.Errorf("%s: content was collapsed anyway:\n%s", name, got)
		}
	}
}

// The reason strip_query exists: Codex names its own build in the query of the
// model list it asks for at startup. The answer does not depend on it, so a
// cassette that stops matching when the agent updates is reporting a change
// that did not happen.
func TestAClientVersionInTheQueryIsNotPartOfTheKey(t *testing.T) {
	before := Normalize("GET", "/v1/models?client_version=0.145.0", nil, queryRules{})
	after := Normalize("GET", "/v1/models?client_version=0.146.0", nil, queryRules{})

	if before.Hash != after.Hash {
		t.Errorf("an agent update changed the key: %s != %s", before.Hash[:12], after.Hash[:12])
	}
	// The target the index records is the one the hash was taken over, or an
	// entry on disk describes a request that would not match it.
	if before.Target != "/v1/models" {
		t.Errorf("Target = %q, want the stripped target", before.Target)
	}
}

// The negative half, and the one that matters more: the query is part of the
// key because it selects provider behaviour. Only the NAMED parameters go —
// Claude Code's `?beta=true` asks for a different surface, and two requests
// that differ in it are two interactions.
func TestOnlyNamedQueryParametersAreStripped(t *testing.T) {
	const body = `{"model":"m","messages":[]}`
	plain := Normalize("POST", "/v1/messages", []byte(body), queryRules{})
	beta := Normalize("POST", "/v1/messages?beta=true", []byte(body), queryRules{})
	if plain.Hash == beta.Hash {
		t.Error("?beta=true was stripped: a beta request and a plain one are not the same interaction")
	}

	// And a stripped parameter takes only itself with it.
	mixed := Normalize("POST", "/v1/messages?beta=true&client_version=0.145.0", []byte(body), queryRules{})
	if mixed.Hash != beta.Hash {
		t.Errorf("stripping client_version disturbed the rest of the query: Target = %q", mixed.Target)
	}
}

// A target with nothing to strip is passed through byte for byte. Re-encoding
// every query would make the key depend on this build's idea of escaping and
// parameter order, which is a way to invalidate cassettes for no reason.
func TestAnUntouchedQueryIsNotRewritten(t *testing.T) {
	const target = "/v1/models?b=2&a=1&x=%2Fslash"
	if got := Normalize("GET", target, nil, queryRules{}).Target; got != target {
		t.Errorf("Target = %q, want it unchanged at %q", got, target)
	}
}

// dropRules is testRules that drops one block, for the case about a client
// that sends a preamble item only sometimes.
type dropRules struct{ testRules }

func (dropRules) DropBlocks() []string { return []string{"<plugins_instructions>"} }

// A block the client includes only sometimes must leave no trace of having
// been there, so the run that sent it keys the same as the run that did not.
//
// Measured on a real Codex session: the recording carried four content items
// and the replay five, and a list whose length differs aligns with nothing.
func TestABlockSentOnlySometimesIsDropped(t *testing.T) {
	body := func(items ...string) []byte {
		var b strings.Builder
		b.WriteString(`{"input":[{"content":[`)
		for i, it := range items {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"text":%q,"type":"input_text"}`, it)
		}
		b.WriteString(`]}]}`)
		return []byte(b.String())
	}

	with := Normalize("POST", "/responses", body("<permissions instructions>...", "<plugins_instructions>\n## Plugins\n...", "<skills_instructions>..."), dropRules{})
	without := Normalize("POST", "/responses", body("<permissions instructions>...", "<skills_instructions>..."), dropRules{})

	if with.Hash != without.Hash {
		t.Errorf("a run that sent the block keys differently:\n  with:    %s\n  without: %s", with.Canonical, without.Canonical)
	}
	if strings.Contains(string(with.Canonical), "plugins_instructions") {
		t.Errorf("the block survived canonicalization:\n%s", with.Canonical)
	}
	// What the block sat between is untouched: dropping an item must not cost
	// the ones around it.
	for _, want := range []string{"permissions instructions", "skills_instructions"} {
		if !strings.Contains(string(with.Canonical), want) {
			t.Errorf("%q went with it:\n%s", want, with.Canonical)
		}
	}
	// And a prompt that merely mentions the tag is discussing it, not being it.
	talking := Normalize("POST", "/responses", body("please explain <plugins_instructions> to me"), dropRules{})
	if !strings.Contains(string(talking.Canonical), "please explain") {
		t.Errorf("a prompt about the block was dropped as the block:\n%s", talking.Canonical)
	}
}
