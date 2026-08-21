package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Alignment compares a recorded request against the live one that is being
// served in its place, and says whether they are the same interaction.
//
// It exists because hashing the whole body makes every byte of it load-bearing,
// and an agent's body is four things stacked together: the conversation, the
// client's own metadata, what it observed locally this run, and its build
// details. Only the first decides the answer. Alignment separates them
// STRUCTURALLY instead of by anticipating every byte the other three can
// produce — see SPEC.md section 5.
//
// The verdicts carry no thresholds. A shape difference is a divergence, a leaf
// difference outside a declared volatile path is a divergence, and a difference
// under one is the world answering differently, which is not cs-vcr's to
// reproduce.
type Alignment struct {
	// Shape is where the two requests are not the same shape: a key on one
	// side only, arrays of different lengths, a value of another type. This is
	// the agent having been asked something different.
	Shape []Difference
	// Leaf is where a value differs at a path nothing declared volatile.
	Leaf []Difference
	// Tolerated is where a value differs under a volatile path. Kept rather
	// than discarded: it is what correlation substitutes, and what a run
	// reports so that a changed world is visible instead of silent.
	Tolerated []Difference
}

// Matches reports whether the live request may be served this recording.
func (a Alignment) Matches() bool { return len(a.Shape) == 0 && len(a.Leaf) == 0 }

// Difference is one place two requests disagree.
type Difference struct {
	// Path is the JSON path, with concrete indices: input[2].output[0].text.
	Path string
	// Recorded and Live are the leaf values. Both nil for a shape difference,
	// which is about the shape rather than about a value.
	Recorded, Live any
	// Why describes a shape difference in the terms a reader needs: "4 items
	// vs 3", "only in the live request".
	Why string
}

// Rule is a JSON path in the same language `strip_fields` uses — dot-separated,
// a `[]` suffix descending into every element of an array. It matches a
// concrete path at or below itself, so `client_metadata` covers every field
// inside it however that object is shaped this run.
type Rule string

// Align walks the two canonical request bodies in parallel.
//
// A body that will not parse is not an alignment failure, it is a caller error:
// both sides come from cs-vcr's own canonicalization, so unparseable JSON here
// means something upstream is broken and should say so rather than report a
// mismatch nobody can act on.
func Align(recorded, live []byte, volatile []Rule) (Alignment, error) {
	a, err := decode(recorded)
	if err != nil {
		return Alignment{}, fmt.Errorf("recorded request is not JSON: %w", err)
	}
	b, err := decode(live)
	if err != nil {
		return Alignment{}, fmt.Errorf("live request is not JSON: %w", err)
	}
	al := &Alignment{}
	al.walk(a, b, "", "", volatile)
	return *al, nil
}

// decode reads a canonical body, treating an empty one as the absence of a
// body rather than as malformed JSON.
//
// A bodiless request is ordinary and has to align with another bodiless one:
// every agent opens a session with probes that carry nothing — Codex asks for
// the model list, Claude Code sends `HEAD /api/hello` — and canonicalizing
// nothing produces nothing.
func decode(b []byte) (any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// path is what a difference is reported at, and carries the index an element
// actually had. rulePath is the same walk spelled for rule matching, where an
// array element that names a role is spelled by that role instead — so
// `messages[role=tool].content` can claim a tool result without claiming the
// prompt beside it. Two paths rather than one because both jobs matter: a
// report that said `messages[role=user]` would stop saying WHICH message.
func (a *Alignment) walk(rec, live any, path, rulePath string, volatile []Rule) {
	// Volatility is decided BEFORE the comparison, and stops the descent. A
	// per-run block like client_metadata differs in its shape as well as its
	// values — a key present one run and absent the next — so a check that
	// only covered leaves would report the world's noise as a divergence.
	if covered(rulePath, volatile) {
		if !equal(rec, live) {
			a.Tolerated = append(a.Tolerated, Difference{Path: path, Recorded: rec, Live: live})
		}
		return
	}

	switch r := rec.(type) {
	case map[string]any:
		l, ok := live.(map[string]any)
		if !ok {
			a.shape(path, rec, live)
			return
		}
		for _, k := range union(r, l) {
			_, inRec := r[k]
			_, inLive := l[k]
			child := join(path, k)
			switch {
			case !inLive:
				a.Shape = append(a.Shape, Difference{Path: child, Why: "only in the recorded request"})
			case !inRec:
				a.Shape = append(a.Shape, Difference{Path: child, Why: "only in the live request"})
			default:
				a.walk(r[k], l[k], child, join(rulePath, k), volatile)
			}
		}
	case []any:
		l, ok := live.([]any)
		if !ok {
			a.shape(path, rec, live)
			return
		}
		// An item left with no content says nothing to the model, and whether
		// it is there at all is not the client's considered decision. Dropping
		// `<plugins_instructions>` is what empties one: the block was the
		// entire content of a developer message, so the strip leaves an item
		// carrying `"content": []` behind. Codex sends that husk on some runs
		// and not others — measured on codex-subscription as `input: 11 items
		// vs 10` — and an item that asks nothing must not be why a list of
		// eleven fails to align with a list of ten.
		//
		// Tolerated here rather than normalized away in the canonical form, on
		// purpose. Removing it there would change every key and force a
		// ruleset bump, which invalidates cassettes that never had the problem;
		// tolerating it here only ever makes possible a match that was not
		// before, so every recording made under this ruleset stays as valid as
		// it was.
		r, l = withoutContentless(r), withoutContentless(l)
		// Length before elements: two lists of different lengths have no
		// element-wise correspondence, and reporting one difference per element
		// would bury the fact that an item was added.
		if len(r) != len(l) {
			a.Shape = append(a.Shape, Difference{Path: path, Why: lengths(r, l)})
			return
		}
		for i := range r {
			a.walk(r[i], l[i], fmt.Sprintf("%s[%d]", path, i), rulePath+role(r[i], i), volatile)
		}
	default:
		if !sameType(rec, live) {
			a.shape(path, rec, live)
			return
		}
		if !equal(rec, live) {
			a.Leaf = append(a.Leaf, Difference{Path: path, Recorded: rec, Live: live})
		}
	}
}

func (a *Alignment) shape(path string, rec, live any) {
	a.Shape = append(a.Shape, Difference{Path: path,
		Why: fmt.Sprintf("%s vs %s", kind(rec), kind(live))})
}

// covered reports whether a rule claims this path or anything above it.
func covered(path string, volatile []Rule) bool {
	if path == "" {
		return false
	}
	for _, r := range volatile {
		if matches(string(r), path) {
			return true
		}
	}
	return false
}

// matches reports whether a rule matches a prefix of a concrete path. The rule
// is dot-separated with `[]` for any index; the concrete path carries the index
// it actually had.
func matches(rule, path string) bool {
	rs, ps := strings.Split(rule, "."), strings.Split(path, ".")
	if len(rs) > len(ps) {
		return false
	}
	for i, seg := range rs {
		if !segMatches(seg, ps[i]) {
			return false
		}
	}
	return true
}

func segMatches(rule, seg string) bool {
	name, inside, bracketed := cutIndex(seg)
	if want, wantInside, ruleBracketed := cutIndex(rule); ruleBracketed {
		if !bracketed || want != name {
			return false
		}
		// `[]` claims every element; anything else claims the elements spelled
		// that way, which is how `messages[role=tool]` reaches a tool result
		// and leaves the prompt alone.
		return wantInside == "" || wantInside == inside
	}
	// A rule naming no index matches the array itself as well as a bare field,
	// so `client_metadata` covers it however it is shaped this run.
	if bracketed {
		return rule == name
	}
	return rule == seg
}

// role spells one array element for rule matching: by the role it names, when
// it names one, and by its index otherwise.
//
// The OpenAI chat surface is why this exists. It carries a tool RESULT as a
// message with role "tool", beside the prompt in the same list — where the
// responses surface has `input[].output` and Anthropic has a nested
// `content[].content`, both reachable by shape alone. Here shape reaches
// either both or neither, and tolerating the prompt is tolerating the question.
func role(elem any, i int) string {
	if m, ok := elem.(map[string]any); ok {
		if r, ok := m["role"].(string); ok && r != "" {
			return "[role=" + r + "]"
		}
	}
	return fmt.Sprintf("[%d]", i)
}

// cutIndex splits `input[2]` or `messages[role=tool]` into its name and
// whatever the brackets held.
func cutIndex(seg string) (name, inside string, ok bool) {
	open := strings.IndexByte(seg, '[')
	if open < 0 || !strings.HasSuffix(seg, "]") {
		return seg, "", false
	}
	return seg[:open], seg[open+1 : len(seg)-1], true
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// Generalize replaces the concrete indices in a path with `[]`, turning one
// observed difference into the rule that would cover every turn of it. It is
// what `calibrate` emits.
func Generalize(path string) string {
	var b strings.Builder
	depth := 0
	for _, ch := range path {
		switch {
		case ch == '[':
			depth++
			b.WriteByte('[')
		case ch == ']':
			depth--
			b.WriteByte(']')
		case depth == 0:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func union(a, b map[string]any) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	// Sorted, so a report reads the same way twice.
	sort.Strings(out)
	return out
}

func equal(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func sameType(a, b any) bool { return kind(a) == kind(b) }

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// Rules turns configured path strings into alignment rules. Configuration
// carries plain strings so a YAML file needs no knowledge of this package;
// this is the one place that knows they are paths.
func Rules(paths []string) []Rule {
	if len(paths) == 0 {
		return nil
	}
	out := make([]Rule, len(paths))
	for i, p := range paths {
		out[i] = Rule(p)
	}
	return out
}

// lengths describes two arrays of different lengths.
//
// The count alone is not enough to act on. A prompt is a list of blocks, and
// "4 items vs 3" sends a reader to open both bodies and count — measured on a
// real session, where the answer turned out to be a `<plugins_instructions>`
// block Codex sent on one run and not the next. Listing what each side holds
// puts the missing item in front of them instead.
//
// Both sides are listed as they are, with nothing matched up: pairing items
// across a length change means guessing which one went, and a guess here would
// name the wrong block with total confidence.
func lengths(rec, live []any) string {
	n := fmt.Sprintf("%d items vs %d", len(rec), len(live))
	// A bound on output, not on meaning: a conversation's message list runs to
	// hundreds, and printing all of them buries the difference again.
	const most = 12
	if len(rec) > most || len(live) > most {
		return n
	}
	return n + "\n    recorded: " + labels(rec) + "\n    this run: " + labels(live)
}

func labels(items []any) string {
	if len(items) == 0 {
		return "(empty)"
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = label(it)
	}
	return strings.Join(out, " | ")
}

// label names one item of a list the way its reader would: by the text it
// carries, since that is what tells one prompt block from another.
func label(v any) string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case map[string]any:
		for _, k := range []string{"text", "content", "type", "role"} {
			if x, ok := t[k].(string); ok && x != "" {
				s = x
				break
			}
		}
		if s == "" {
			s = "object"
		}
	default:
		s = kind(v)
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if len(s) > 28 {
		return s[:28] + "…"
	}
	return s
}

// Short renders a value for a one-line report. A recorded value can be a whole
// tool output, and a message that pastes 8 KB into a terminal is one nobody
// reads to the end of.
func Short(v any) string {
	s, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return "?"
		}
		s = string(b)
	}
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// withoutContentless drops list items whose `content` is present and empty.
//
// Only that exact shape: a map with a `content` key holding an empty list. An
// item with no `content` key at all is a different thing entirely — a tool
// call, a function output — and one whose content is an empty STRING was a
// deliberate empty message. Neither is a husk left behind by a strip.
//
// The list is returned unchanged when it holds none, so the ordinary path
// allocates nothing.
func withoutContentless(items []any) []any {
	n := 0
	for _, it := range items {
		if contentless(it) {
			n++
		}
	}
	if n == 0 {
		return items
	}
	kept := make([]any, 0, len(items)-n)
	for _, it := range items {
		if !contentless(it) {
			kept = append(kept, it)
		}
	}
	return kept
}

func contentless(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	c, ok := m["content"]
	if !ok {
		return false
	}
	list, ok := c.([]any)
	return ok && len(list) == 0
}
