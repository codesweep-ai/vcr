package cassette

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// CoalesceToolInput rewrites a recorded SSE stream so each run of fragments a
// client reassembles arrives as one event instead of many.
//
// A model streams a tool call's arguments as fragments split at arbitrary
// character boundaries. Every surface does it, in its own spelling:
//
//	anthropic.messages  {"delta":{"type":"input_json_delta","partial_json":": \"/tm"}}
//	openai.responses    {"type":"response.custom_tool_call_input.delta","delta":"/tm"}
//	openai.chat         {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"/tm"}}]}}]}
//
// Nothing can be substituted in that. A file path, a dispatch id, anything
// specific to the machine that recorded it, is spread across chunks — so a
// recording carries the recording machine's values into a replay that has to
// use its own, and the agent then acts on a path that does not exist. Measured
// on a real OpenCode session: the model asked to write `<root>/work/hello.txt`,
// the path arrived in nineteen pieces, and the replayed run was handed the
// recording machine's directory.
//
// Joining the fragments makes the value contiguous, which is what lets
// normalization reach it. The client concatenates these fragments and parses
// the result, so where they were split is not observable to it: the events
// remain in order, each still one event, and only the joined field arrives in
// one piece rather than thirty.
//
// The same is done to a model's REASONING, and for the same reason: a client
// carries it into the next request as conversation state, and OpenCode against
// Kimi K3 put the working directory in it. Measured — the replayed run was
// handed the recording machine's directory in the middle of a sentence.
//
// It is NOT done to the visible answer. That text is rendered to a person as it
// arrives, and its event boundaries are the fidelity a replayed session is for.
// A path inside it is a path the model is telling someone about, not one a
// client will act on.
//
// A stream with no tool call is returned unchanged, as is anything that does
// not parse — a recorder that mangles what it does not understand is worse than
// one that records it verbatim.
func CoalesceToolInput(stream []byte) []byte {
	if !hasToolFragments(stream) {
		return stream
	}
	var out bytes.Buffer
	// Pending fragments, in the order their runs began. Order, rather than a
	// map: two tool calls streamed at once are flushed together, and a map would
	// emit them in whichever order Go felt like — a recorded stream that differs
	// between two runs of the recorder.
	var pending []*pendingInput

	flushAll := func() {
		for _, p := range pending {
			out.Write(p.render())
		}
		pending = nil
	}
	find := func(key string) *pendingInput {
		for _, p := range pending {
			if p.key == key {
				return p
			}
		}
		return nil
	}

	for _, ev := range splitEvents(stream) {
		data, ok := eventData(ev)
		if !ok {
			flushAll()
			out.Write(ev)
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			flushAll()
			out.Write(ev)
			continue
		}
		frag, ok := toolFragment(msg)
		if !ok {
			// Any other event ends every run of fragments, so the joined events
			// land where the first fragment of each run was.
			flushAll()
			out.Write(ev)
			continue
		}
		p := find(frag.key)
		if p == nil {
			p = &pendingInput{key: frag.key, frame: ev, msg: msg, set: frag.set}
			pending = append(pending, p)
		}
		p.text.WriteString(frag.text)
	}
	// A stream cut short mid-tool-call still gets what it had.
	flushAll()
	return out.Bytes()
}

// hasToolFragments is the cheap check that a stream is worth parsing, naming
// each surface's marker.
func hasToolFragments(stream []byte) bool {
	for _, marker := range [][]byte{
		[]byte("input_json_delta"),  // anthropic.messages, tool arguments
		[]byte("thinking_delta"),    // anthropic.messages, reasoning
		[]byte("_input.delta"),      // openai.responses, custom tools
		[]byte("_arguments.delta"),  // openai.responses, function tools
		[]byte("_text.delta"),       // openai.responses, reasoning
		[]byte("tool_calls"),        // openai.chat, tool arguments
		[]byte("reasoning_content"), // openai.chat, reasoning
	} {
		if bytes.Contains(stream, marker) {
			return true
		}
	}
	return false
}

// fragment is one event's worth of a tool call's arguments: which call it
// belongs to, the text, and how to put the joined text back.
type fragment struct {
	key  string
	text string
	set  func(msg map[string]any, joined string)
}

// toolFragment recognizes an argument fragment in any of the three surfaces.
//
// An event that carries anything else as well is not one: it has content the
// client acts on, so it is written through untouched rather than folded into a
// neighbour.
func toolFragment(msg map[string]any) (fragment, bool) {
	// anthropic.messages: content_block_delta carrying input_json_delta.
	if typ, _ := msg["type"].(string); typ == "content_block_delta" {
		idx, hasIdx := msg["index"].(float64)
		d, _ := msg["delta"].(map[string]any)
		if !hasIdx || d == nil {
			return fragment{}, false
		}
		// The tool arguments and the thinking a block carries are the same
		// case in two spellings, and a block is one or the other.
		field := ""
		switch dt, _ := d["type"].(string); dt {
		case "input_json_delta":
			field = "partial_json"
		case "thinking_delta":
			field = "thinking"
		default:
			return fragment{}, false
		}
		text, _ := d[field].(string)
		return fragment{
			key:  "block:" + strconv.FormatFloat(idx, 'f', -1, 64),
			text: text,
			set: func(m map[string]any, joined string) {
				if d, _ := m["delta"].(map[string]any); d != nil {
					d[field] = joined
				}
			},
		}, true
	}
	// openai.responses: one delta event per fragment, addressed by the item it
	// belongs to. Both tool shapes spell the event differently and carry the
	// fragment identically.
	switch typ, _ := msg["type"].(string); typ {
	case "response.custom_tool_call_input.delta", "response.function_call_arguments.delta",
		"response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		text, ok := msg["delta"].(string)
		if !ok {
			return fragment{}, false
		}
		item, _ := msg["item_id"].(string)
		out, _ := msg["output_index"].(float64)
		sum, _ := msg["summary_index"].(float64)
		return fragment{
			key: "item:" + typ + ":" + item + ":" +
				strconv.FormatFloat(out, 'f', -1, 64) + ":" + strconv.FormatFloat(sum, 'f', -1, 64),
			text: text,
			set:  func(m map[string]any, joined string) { m["delta"] = joined },
		}, true
	}
	// openai.chat: the fragment is buried in the first choice's delta. Only a
	// chunk holding exactly one tool call and no text is folded — a chunk with
	// two would need the arguments of both joined into one frame, and the models
	// that stream parallel calls send them one chunk at a time anyway.
	choices, _ := msg["choices"].([]any)
	if len(choices) != 1 {
		return fragment{}, false
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return fragment{}, false
	}
	d, _ := choice["delta"].(map[string]any)
	if d == nil {
		return fragment{}, false
	}
	if text, _ := d["content"].(string); text != "" {
		return fragment{}, false
	}
	calls, _ := d["tool_calls"].([]any)
	if len(calls) == 0 {
		// No tool call: the other thing this surface streams in pieces is the
		// reasoning, under a key the provider chose for itself.
		if text, ok := d["reasoning_content"].(string); ok {
			ci, _ := choice["index"].(float64)
			return fragment{
				key:  "reason:" + strconv.FormatFloat(ci, 'f', -1, 64),
				text: text,
				set: func(m map[string]any, joined string) {
					if d := chatDelta(m); d != nil {
						d["reasoning_content"] = joined
					}
				},
			}, true
		}
		return fragment{}, false
	}
	if len(calls) != 1 {
		return fragment{}, false
	}
	call, _ := calls[0].(map[string]any)
	if call == nil {
		return fragment{}, false
	}
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return fragment{}, false
	}
	args, ok := fn["arguments"].(string)
	if !ok {
		return fragment{}, false
	}
	ci, _ := choice["index"].(float64)
	ti, _ := call["index"].(float64)
	return fragment{
		key: "call:" + strconv.FormatFloat(ci, 'f', -1, 64) + ":" + strconv.FormatFloat(ti, 'f', -1, 64),
		// The first chunk of a call carries the id and the name with an empty
		// argument string; it is the frame the rest are joined into.
		text: args,
		set: func(m map[string]any, joined string) {
			d := chatDelta(m)
			if d == nil {
				return
			}
			calls, _ := d["tool_calls"].([]any)
			if len(calls) == 0 {
				return
			}
			if call, _ := calls[0].(map[string]any); call != nil {
				if fn, _ := call["function"].(map[string]any); fn != nil {
					fn["arguments"] = joined
				}
			}
		},
	}, true
}

// chatDelta reaches the first choice's delta of an openai.chat chunk, which is
// where everything that surface streams in pieces lives.
func chatDelta(m map[string]any) map[string]any {
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil
	}
	d, _ := choice["delta"].(map[string]any)
	return d
}

type pendingInput struct {
	key   string
	frame []byte         // the first fragment's event, reused for its `event:` line
	msg   map[string]any // its parsed data, rewritten with the joined text
	set   func(map[string]any, string)
	text  bytes.Buffer
}

// render rebuilds the event with all fragments joined into one.
func (p *pendingInput) render() []byte {
	p.set(p.msg, p.text.String())
	data, err := json.Marshal(p.msg)
	if err != nil {
		return p.frame
	}
	var out bytes.Buffer
	// Keep every line of the original frame except the data line, so the
	// `event:` name and any comment survive exactly.
	for _, line := range bytes.SplitAfter(p.frame, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		out.Write(line)
	}
	// The data line goes back where SSE requires it: before the blank line that
	// terminates the event.
	body := bytes.TrimRight(out.Bytes(), "\n")
	var ev bytes.Buffer
	if len(body) > 0 {
		ev.Write(body)
		ev.WriteByte('\n')
	}
	ev.WriteString("data: ")
	ev.Write(data)
	ev.WriteString("\n\n")
	return ev.Bytes()
}

// splitEvents cuts a stream into events, each keeping its terminator so the
// pieces concatenate back into the original bytes.
func splitEvents(stream []byte) [][]byte {
	var out [][]byte
	for len(stream) > 0 {
		i := bytes.Index(stream, []byte("\n\n"))
		if i < 0 {
			out = append(out, stream)
			break
		}
		out = append(out, stream[:i+2])
		stream = stream[i+2:]
	}
	return out
}

// eventData returns the payload of an event's `data:` line.
func eventData(ev []byte) ([]byte, bool) {
	for line := range bytes.SplitSeq(ev, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			return bytes.TrimSpace(rest), true
		}
	}
	return nil, false
}

// CoalesceTextSpanning joins a model's VISIBLE answer, but only across the
// events some value that must be substituted straddles.
//
// CoalesceToolInput above deliberately leaves the visible answer alone: its
// event boundaries are what a replayed session reproduces for a person
// watching, and a path inside it is usually one the model is describing rather
// than one a client will act on.
//
// That holds until an agent is the reader. Under cs-campaign the member writes
// its own answer to a file and the next turn carries the file back as
// conversation state, so a run-specific name inside the visible text IS acted
// on — and, split across deltas, it is invisible to the substitution that
// should have blanked it. Measured on opencode-fireworks: the orchestrator's
// readback summary named `orchestrator-5235a8fe`, the recording's own sandbox,
// spread across delta events. It survived into the cassette, the replay served
// it back, and the member then held two different orchestrator names — the
// recording's in its summary and this run's in its environment. The capture
// numbered them in order of appearance, the summary's became <ORCHESTRATOR:2>
// where the recording had <ORCHESTRATOR:1>, and every later request missed.
//
// So this joins the minimum: a run of text fragments is folded into one event
// only when the joined text actually contains one of the values, and is
// written through untouched otherwise. The pacing of ordinary prose survives,
// which is what CoalesceToolInput's reasoning was protecting; the few events a
// name lands in do not, which is the point.
func CoalesceTextSpanning(stream []byte, values []string) []byte {
	needles := make([][]byte, 0, len(values))
	for _, v := range values {
		if v != "" {
			needles = append(needles, []byte(v))
		}
	}
	if len(needles) == 0 || !hasTextFragments(stream) {
		return stream
	}
	var out bytes.Buffer
	var pending *pendingInput
	var raw [][]byte // the pending run's original events, kept for the untouched case

	flush := func() {
		if pending == nil {
			return
		}
		joined := pending.text.Bytes()
		folded := false
		for _, n := range needles {
			if bytes.Contains(joined, n) {
				folded = true
				break
			}
		}
		if folded {
			out.Write(pending.render())
		} else {
			for _, ev := range raw {
				out.Write(ev)
			}
		}
		pending, raw = nil, nil
	}

	for _, ev := range splitEvents(stream) {
		data, ok := eventData(ev)
		if !ok {
			flush()
			out.Write(ev)
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			flush()
			out.Write(ev)
			continue
		}
		frag, ok := textFragment(msg)
		if !ok {
			flush()
			out.Write(ev)
			continue
		}
		if pending != nil && pending.key != frag.key {
			flush()
		}
		if pending == nil {
			pending = &pendingInput{key: frag.key, frame: ev, msg: msg, set: frag.set}
		}
		pending.text.WriteString(frag.text)
		raw = append(raw, ev)
	}
	flush()
	return out.Bytes()
}

// hasTextFragments is the cheap check that a stream carries visible answer
// text at all, in any of the three surfaces' spellings.
func hasTextFragments(stream []byte) bool {
	for _, marker := range [][]byte{
		[]byte("text_delta"),        // anthropic.messages
		[]byte("output_text.delta"), // openai.responses
		[]byte(`"content"`),         // openai.chat
	} {
		if bytes.Contains(stream, marker) {
			return true
		}
	}
	return false
}

// textFragment recognizes a fragment of the model's visible answer, in the
// same three surfaces toolFragment covers. An event carrying anything besides
// the text — a tool call, a finish reason — is not one, so it is written
// through rather than folded into a neighbour.
func textFragment(msg map[string]any) (fragment, bool) {
	// anthropic.messages: content_block_delta carrying text_delta.
	if typ, _ := msg["type"].(string); typ == "content_block_delta" {
		idx, hasIdx := msg["index"].(float64)
		d, _ := msg["delta"].(map[string]any)
		if !hasIdx || d == nil {
			return fragment{}, false
		}
		if dt, _ := d["type"].(string); dt != "text_delta" {
			return fragment{}, false
		}
		text, _ := d["text"].(string)
		return fragment{
			key:  "text-block:" + strconv.FormatFloat(idx, 'f', -1, 64),
			text: text,
			set: func(m map[string]any, joined string) {
				if d, _ := m["delta"].(map[string]any); d != nil {
					d["text"] = joined
				}
			},
		}, true
	}
	// openai.responses: one delta event per fragment of the output text.
	if typ, _ := msg["type"].(string); typ == "response.output_text.delta" {
		text, ok := msg["delta"].(string)
		if !ok {
			return fragment{}, false
		}
		item, _ := msg["item_id"].(string)
		out, _ := msg["output_index"].(float64)
		return fragment{
			key:  "text-item:" + item + ":" + strconv.FormatFloat(out, 'f', -1, 64),
			text: text,
			set:  func(m map[string]any, joined string) { m["delta"] = joined },
		}, true
	}
	// openai.chat: the text is the first choice's delta.content, and that must
	// be all it carries.
	choices, _ := msg["choices"].([]any)
	if len(choices) != 1 {
		return fragment{}, false
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return fragment{}, false
	}
	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		return fragment{}, false
	}
	d, _ := choice["delta"].(map[string]any)
	if d == nil {
		return fragment{}, false
	}
	if _, hasTools := d["tool_calls"]; hasTools {
		return fragment{}, false
	}
	text, ok := d["content"].(string)
	if !ok {
		return fragment{}, false
	}
	idx, _ := choice["index"].(float64)
	return fragment{
		key:  "text-choice:" + strconv.FormatFloat(idx, 'f', -1, 64),
		text: text,
		set: func(m map[string]any, joined string) {
			cs, _ := m["choices"].([]any)
			if len(cs) != 1 {
				return
			}
			c, _ := cs[0].(map[string]any)
			if c == nil {
				return
			}
			if dd, _ := c["delta"].(map[string]any); dd != nil {
				dd["content"] = joined
			}
		},
	}, true
}
