package cassette

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// Key is the hash a request is matched by, and the canonical form it was
// computed from.
//
// The canonical form is kept because it is what gets written to req/<hash>.json
// and what a miss diff is computed against: a hash alone tells a reader that
// two requests differ, which is the least useful thing anyone could be told.
type Key struct {
	Hash      string
	Canonical []byte
	// Target is the request target the hash was computed over, which is the
	// one the index records: an entry whose path disagrees with its key
	// describes a request that would not match it.
	Target string
	// Captured is what this request's run-specific patterns matched, for
	// restoring into the response that answers it.
	Captured map[string]string
}

// Normalize canonicalizes a request and hashes it.
//
// Two requests match when the model would answer them identically. That is a
// semantic claim, not a syntactic one, which is why the strip list is versioned
// configuration rather than a constant: `cache_control` markers move with a
// client release and tool-use IDs change every run, and neither changes what
// the model is being asked.
//
// The method and the full request target — path AND query — are part of the
// key, not just the body. They have to be: a bodiless GET hashes to the SHA-256
// of the empty string, so keying on the body alone makes every such request to
// every endpoint the same entry; and Claude Code asks for beta behaviour with
// `?beta=true`, so one body under two queries is two interactions a provider
// answers differently.
//
// Which is why the query has a strip list of its own: a parameter naming the
// client's own build is in the target for the provider's telemetry, not because
// it changes the answer. Codex opens every session with
// `GET /v1/models?client_version=0.145.0`, so without it a cassette stops
// matching the day the agent updates — a miss that reads as a changed prompt
// and is not one.
//
// The canonical form kept for the file on disk is the BODY only, so a request
// diff in review shows the prompt and not a synthetic header line.
//
// A body that is not JSON is hashed verbatim. Refusing it would make cs-vcr
// unable to record a surface it does not model, and it has to record those:
// a request it proxies but does not record is one replay can never serve.
func Normalize(method, path string, body []byte, rules Ruleset) Key {
	target := stripQuery(path, rules.StripQuery())
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		canon, captured := rules.Apply(body)
		return Key{Hash: hashOf(method, target, canon), Canonical: canon, Target: target, Captured: captured}
	}
	for _, field := range rules.StripFields() {
		v = strip(v, strings.Split(field, "."))
	}
	v = sortToolResults(v)
	v = collapseSingleTextBlocks(v)
	// json.Marshal sorts object keys, which is the canonicalization the format
	// requires; the indented form is what lands on disk, so the file a reviewer
	// reads is the exact bytes that were hashed.
	canon, err := marshalCanonical(v)
	if err != nil {
		return Key{Hash: hashOf(method, target, body), Canonical: body, Target: target}
	}
	// Substitutions run on the canonical TEXT rather than on a walk of the
	// tree, because what they target is prose inside a string value and a
	// walker would have to reassemble it anyway. It also means the file written
	// to disk shows the normalized form, so a reviewer sees `<DATE>` and knows
	// the entry does not depend on the day it was recorded.
	canon, captured := rules.Apply(canon)
	return Key{Hash: hashOf(method, target, canon), Canonical: canon, Target: target, Captured: captured}
}

// marshalCanonical renders the canonical form: keys sorted, indented, and with
// `<`, `>` and `&` left as themselves.
//
// json.Marshal escapes those three for embedding in HTML, which nothing here
// does. The cost is paid twice over. A prompt is full of them — Codex frames
// its whole environment context in XML-ish tags — so the escaping turns the
// artifact this format exists to make reviewable into
// `<environment_context>`. And it silently defeats the ruleset: a
// `replace` pattern is written against the prompt as the client sent it, so a
// rule naming `<current_date>` matched nothing and gave no hint why.
func marshalCanonical(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode terminates with a newline that MarshalIndent does not write, and
	// the canonical bytes are hashed: leaving it in would be a difference
	// between two builds that agree on everything else.
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// stripQuery removes the named parameters from a request target.
//
// Only a target that carries one of them is rewritten. The rest are returned
// byte for byte, because re-encoding a query is not free of meaning: the order
// of parameters and the exact escaping are the client's, and a rule aimed at
// one parameter has no business normalizing every other request's target.
func stripQuery(target string, names []string) string {
	path, raw, ok := strings.Cut(target, "?")
	if !ok || len(names) == 0 {
		return target
	}
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	kept := make([]string, 0, strings.Count(raw, "&")+1)
	stripped := false
	for param := range strings.SplitSeq(raw, "&") {
		name, _, _ := strings.Cut(param, "=")
		// The name is compared decoded, so a client that escapes it is not a
		// way around the rule.
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}
		if drop[name] {
			stripped = true
			continue
		}
		kept = append(kept, param)
	}
	if !stripped {
		return target
	}
	if len(kept) == 0 {
		return path
	}
	return path + "?" + strings.Join(kept, "&")
}

// sortToolResults puts a message's tool_result blocks in a fixed order.
//
// A model can call several tools in one turn, and the client returns their
// results in whatever order they finish — a small file before a large one on
// one run, the other way round on the next. That flipped ordering made a real
// campaign miss on replay while every byte of content was identical.
//
// Reordering is safe because the order carries no information: each result
// names the call it answers with tool_use_id, which is what binds them. This is
// the same kind of canonicalization as sorting object keys, and like that one it
// touches only the form hashed and stored — the body forwarded upstream is the
// client's own bytes, untouched.
//
// Only the tool_result blocks move. Anything else in the array stays where it
// was, because for other block types position IS meaning.
// collapseSingleTextBlocks rewrites a content of exactly one plain text block
// to the bare string, which the API treats as the same message.
//
// The client chooses between the two forms on its own: Claude Code sends one
// message as `"content": "Available agent types…"` on one run and as
// `"content": [{"type":"text","text":"Available agent types…"}]` on the next,
// with the text identical. The difference means nothing to the provider, and no
// rule a user could write would reach it — `replace` operates on bytes, and
// this is a change of shape.
//
// Only a lone plain text block collapses. A block carrying anything else is
// left alone: the extra field is either meaningful or something `strip_fields`
// was meant to remove, and guessing which is not this function's business.
func collapseSingleTextBlocks(v any) any {
	root, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if s, ok := loneText(root["system"]); ok {
		root["system"] = s
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return v
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := loneText(msg["content"]); ok {
			msg["content"] = s
		}
	}
	return v
}

// loneText reports the string of a content that is exactly one plain text
// block, and nothing else.
func loneText(content any) (string, bool) {
	blocks, ok := content.([]any)
	if !ok || len(blocks) != 1 {
		return "", false
	}
	blk, ok := blocks[0].(map[string]any)
	if !ok || blk["type"] != "text" {
		return "", false
	}
	text, ok := blk["text"].(string)
	if !ok || len(blk) != 2 {
		return "", false
	}
	return text, true
}

func sortToolResults(v any) any {
	root, ok := v.(map[string]any)
	if !ok {
		return v
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return v
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		var at []int
		var blocks []any
		for i, b := range content {
			if blk, ok := b.(map[string]any); ok && blk["type"] == "tool_result" {
				at = append(at, i)
				blocks = append(blocks, b)
			}
		}
		if len(blocks) < 2 {
			continue
		}
		sort.SliceStable(blocks, func(i, j int) bool {
			return toolUseID(blocks[i]) < toolUseID(blocks[j])
		})
		for n, i := range at {
			content[i] = blocks[n]
		}
	}
	return v
}

func toolUseID(b any) string {
	if blk, ok := b.(map[string]any); ok {
		if id, ok := blk["tool_use_id"].(string); ok {
			return id
		}
	}
	return ""
}

// Ruleset is what normalization needs from the configuration, so this package
// does not depend on the config package.
type Ruleset interface {
	StripFields() []string
	// StripQuery names the query parameters removed from the request target
	// before it is hashed.
	StripQuery() []string
	// Apply normalizes canonical request text and reports what its
	// run-specific captures matched.
	Apply([]byte) ([]byte, map[string]string)
}

// hashOf keys an interaction on what identifies it: where it was going, and
// what it said.
func hashOf(method, path string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{' '})
	h.Write([]byte(path))
	h.Write([]byte{'\n'})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// strip removes one field path from a decoded body. A segment ending in []
// descends into every element of an array, which is what the interesting paths
// need: `messages[].content[].cache_control` marks a field that appears once
// per content block, at a depth nobody wants to enumerate.
func strip(v any, path []string) any {
	if len(path) == 0 {
		return nil
	}
	seg := path[0]
	rest := path[1:]

	if before, ok := strings.CutSuffix(seg, "[]"); ok {
		name := before
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		arr, ok := m[name].([]any)
		if !ok {
			return v
		}
		for i, el := range arr {
			if len(rest) == 0 {
				arr[i] = nil
			} else {
				arr[i] = strip(el, rest)
			}
		}
		return m
	}

	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if len(rest) == 0 {
		delete(m, seg)
		return m
	}
	if child, ok := m[seg]; ok {
		m[seg] = strip(child, rest)
	}
	return m
}
