package cassette

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// A tool call as a model actually streams it: the arguments split at arbitrary
// character boundaries, straight through the middle of a path.
const fragmentedToolCall = `event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"Read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"f"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ile_path\": \"/ho"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"me/ada/proj/R"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"EADME.md\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

`

// The whole point: after coalescing, a value that was split across chunks is
// contiguous, so normalization can reach it. Before this, a recorded response
// carried the recording machine's paths into every replay.
func TestCoalesceMakesAToolArgumentSubstitutable(t *testing.T) {
	got := CoalesceToolInput([]byte(fragmentedToolCall))
	if !strings.Contains(string(got), `/home/ada/proj/README.md`) {
		t.Fatalf("the path is still split across events:\n%s", got)
	}
	// And the joined JSON is intact, not merely concatenated text.
	var input struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(joinedPartial(t, got)), &input); err != nil {
		t.Fatalf("joined partial_json does not parse: %v", err)
	}
	if input.FilePath != "/home/ada/proj/README.md" {
		t.Errorf("file_path = %q", input.FilePath)
	}
}

// The client concatenates fragments, so joining them is invisible to it — but
// only if the events stay in order and the surrounding ones are untouched.
func TestCoalescePreservesEventOrderAndFrames(t *testing.T) {
	got := string(CoalesceToolInput([]byte(fragmentedToolCall)))
	order := []string{"content_block_start", "input_json_delta", "content_block_stop"}
	at := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 {
			t.Fatalf("%s missing from the rewritten stream:\n%s", want, got)
		}
		if i < at {
			t.Fatalf("%s is out of order:\n%s", want, got)
		}
		at = i
	}
	if n := strings.Count(got, "input_json_delta"); n != 1 {
		t.Errorf("fragments were not joined into one event: %d remain", n)
	}
	if !strings.HasPrefix(got, "event: content_block_start\n") {
		t.Errorf("the first event's frame was altered:\n%.80s", got)
	}
	// Every event still ends on a boundary, or replay would not frame them.
	for _, ev := range strings.SplitAfter(strings.TrimRight(got, "\n"), "\n\n") {
		if ev != "" && !strings.Contains(ev, "data:") {
			t.Errorf("event without a data line: %q", ev)
		}
	}
}

// A stream with no tool call must come back byte for byte. Most responses are
// this, and a recorder that rewrites what it does not need to is a recorder
// that will eventually rewrite something it should not have.
func TestCoalesceLeavesOrdinaryTextStreamsAlone(t *testing.T) {
	const text = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: message_stop
data: {"type":"message_stop"}

`
	if got := string(CoalesceToolInput([]byte(text))); got != text {
		t.Errorf("an ordinary stream was rewritten:\n--- got ---\n%s\n--- want ---\n%s", got, text)
	}
}

// Two tool calls in one response are separate blocks and must not be merged
// into each other.
func TestCoalesceKeepsBlocksSeparate(t *testing.T) {
	two := fragmentedToolCall +
		strings.ReplaceAll(fragmentedToolCall, `"index":1`, `"index":2`)
	got := string(CoalesceToolInput([]byte(two)))
	if n := strings.Count(got, "input_json_delta"); n != 2 {
		t.Errorf("expected one joined delta per block, got %d", n)
	}
}

// Anything unparseable passes through: a recorder that mangles what it does not
// understand is worse than one that records it verbatim.
func TestCoalescePassesThroughUnparseableEvents(t *testing.T) {
	junk := "event: weird\ndata: not json at all\n\n" + fragmentedToolCall
	got := string(CoalesceToolInput([]byte(junk)))
	if !strings.Contains(got, "data: not json at all") {
		t.Errorf("an unparseable event was dropped:\n%s", got)
	}
}

// A stream cut off mid-tool-call still yields what it had, because a truncated
// stream is a real thing to reproduce — it is how a cancelled request looks.
func TestCoalesceFlushesATruncatedToolCall(t *testing.T) {
	cut := strings.Split(fragmentedToolCall, "event: content_block_stop")[0]
	got := string(CoalesceToolInput([]byte(cut)))
	if !strings.Contains(got, "/home/ada/proj/R") {
		t.Errorf("a truncated tool call lost its fragments:\n%s", got)
	}
}

// joinedPartial pulls the single joined partial_json back out for assertion.
func joinedPartial(t *testing.T, stream []byte) string {
	t.Helper()
	for _, ev := range splitEvents(stream) {
		data, ok := eventData(ev)
		if !ok {
			continue
		}
		var msg struct {
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Delta.Type == "input_json_delta" {
			return msg.Delta.PartialJSON
		}
	}
	t.Fatal("no input_json_delta in the rewritten stream")
	return ""
}

// The same defect in the other two spellings, both measured against real
// sessions: OpenCode through an OpenAI-shaped provider streams a path in
// nineteen pieces, and Codex streams its tool input in twenty-nine.
//
// The Anthropic shape was the only one joined for a while, and the gap was
// invisible until an agent wrote to an absolute path: the replayed run was
// handed the recording machine's directory and asked to write there.
func TestCoalesceJoinsEveryStreamedToolShape(t *testing.T) {
	cases := []struct {
		name, stream, want string
	}{{
		name: "openai.chat",
		stream: `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":""}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/ho"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"me/ada/x\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

`,
		want: `{"path":"/home/ada/x"}`,
	}, {
		name: "openai.responses",
		stream: `data: {"type":"response.custom_tool_call_input.delta","delta":"cat /ho","item_id":"ctc_1","output_index":1}

data: {"type":"response.custom_tool_call_input.delta","delta":"me/ada/x","item_id":"ctc_1","output_index":1}

data: {"type":"response.custom_tool_call_input.done","input":"cat /home/ada/x","item_id":"ctc_1","output_index":1}

`,
		want: `cat /home/ada/x`,
	}, {
		name: "openai.responses function call",
		stream: `data: {"type":"response.function_call_arguments.delta","delta":"{\"p\":\"/ho","item_id":"fc_1","output_index":0}

data: {"type":"response.function_call_arguments.delta","delta":"me/ada/x\"}","item_id":"fc_1","output_index":0}

data: {"type":"response.output_item.done","output_index":0}

`,
		want: `{"p":"/home/ada/x"}`,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(CoalesceToolInput([]byte(tc.stream)))
			if !strings.Contains(got, jsonEscape(tc.want)) {
				t.Errorf("the value is still split across events:\n%s", got)
			}
			// The events around it are untouched, and the stream still ends
			// where it did: a client reads these in order.
			if !strings.Contains(got, "done") && !strings.Contains(got, "finish_reason") {
				t.Errorf("the closing event was consumed:\n%s", got)
			}
		})
	}
}

// jsonEscape renders a value as it appears inside a JSON string, which is where
// the joined fragments end up.
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return strings.Trim(string(b), `"`)
}

// Two tool calls streamed at once are flushed in the order their runs began.
// Map order would make the recorded stream differ between two runs of the
// recorder, which is a cassette that fails review for no reason.
func TestCoalesceFlushesParallelCallsInOrder(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{\"a\":1}"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"read","arguments":"{\"b\":2}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

`
	first := CoalesceToolInput([]byte(stream))
	for range 20 {
		if got := CoalesceToolInput([]byte(stream)); string(got) != string(first) {
			t.Fatalf("two runs of the recorder produced different streams:\n%s\n---\n%s", first, got)
		}
	}
	a, b := strings.Index(string(first), `"a"`), strings.Index(string(first), `"b"`)
	if a < 0 || b < 0 || a > b {
		t.Errorf("the calls were not flushed in the order they started:\n%s", first)
	}
}

// A model's reasoning is streamed in pieces too, and a client carries it into
// the next request as conversation state. Measured on OpenCode against Kimi K3:
// the reasoning said which directory it was working in, the sentence arrived in
// fragments, and the replayed run was handed the recording machine's path in
// the middle of it.
func TestCoalesceJoinsStreamedReasoning(t *testing.T) {
	cases := []struct{ name, stream string }{{
		name: "openai.chat",
		stream: `data: {"choices":[{"index":0,"delta":{"reasoning_content":"working in /ho"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":"me/ada/x now"}}]}

data: {"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}

`,
	}, {
		name: "openai.responses",
		stream: `data: {"type":"response.reasoning_text.delta","delta":"working in /ho","item_id":"rs_1","output_index":0}

data: {"type":"response.reasoning_text.delta","delta":"me/ada/x now","item_id":"rs_1","output_index":0}

data: {"type":"response.output_item.done","output_index":0}

`,
	}, {
		name: "anthropic.messages",
		stream: `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"working in /ho"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"me/ada/x now"}}

data: {"type":"content_block_stop","index":0}

`,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(CoalesceToolInput([]byte(tc.stream)))
			if !strings.Contains(got, "/home/ada/x") {
				t.Errorf("the path is still split across events:\n%s", got)
			}
		})
	}
}

// The visible answer is left in pieces. It is rendered to a person as it
// arrives, and those boundaries are the fidelity a replayed session exists for
// — a path inside it is one the model is describing, not one a client acts on.
func TestCoalesceLeavesTheVisibleAnswerInPieces(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"content":"I wrote /ho"}}]}

data: {"choices":[{"index":0,"delta":{"content":"me/ada/x"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	if got := string(CoalesceToolInput([]byte(stream))); got != stream {
		t.Errorf("a text stream was rewritten:\n%s", got)
	}
}

// The visible answer is left in pieces until a value that must be substituted
// straddles them.
//
// Measured on opencode-fireworks: the orchestrator's readback summary named
// `orchestrator-5235a8fe`, the RECORDING's own sandbox, and the name arrived
// across delta events. Nothing could blank it, the cassette kept it, and the
// replay handed the member the recording's identity beside its own.
func TestCoalesceTextSpanningFoldsOnlyTheRunCarryingAValue(t *testing.T) {
	chunk := func(s string) string {
		return `data: {"choices":[{"index":0,"delta":{"content":` + strconv.Quote(s) + `}}]}` + "\n\n"
	}
	stream := []byte(
		chunk("Ordinary ") + chunk("prose ") + chunk("first.") +
			`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n" +
			chunk("branch is orch") + chunk("estrator-5235") + chunk("a8fe now"))

	got := CoalesceTextSpanning(stream, []string{"orchestrator-5235a8fe"})
	if !bytes.Contains(got, []byte("orchestrator-5235a8fe")) {
		t.Fatalf("the value is still split, so nothing can substitute it:\n%s", got)
	}
	// The prose before it keeps its own pacing: three chunks in, three out.
	if n := bytes.Count(got, []byte(`"content":"Ordinary "`)); n != 1 {
		t.Errorf("the untouched run was rewritten (%d), want it verbatim:\n%s", n, got)
	}
	if !bytes.Contains(got, []byte(`"content":"prose "`)) {
		t.Errorf("an untouched chunk was folded into its neighbour:\n%s", got)
	}
}

// No value to reach means nothing to fold, and a stream carrying none must
// come back byte for byte — the pacing of an answer is what a replayed session
// reproduces for whoever is watching it.
func TestCoalesceTextSpanningLeavesAStreamWithoutAValueAlone(t *testing.T) {
	stream := []byte(
		`data: {"choices":[{"index":0,"delta":{"content":"all "}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{"content":"ordinary"}}]}` + "\n\n")
	if got := CoalesceTextSpanning(stream, []string{"orchestrator-5235a8fe"}); !bytes.Equal(got, stream) {
		t.Errorf("a stream with nothing to substitute was rewritten:\n%s", got)
	}
	if got := CoalesceTextSpanning(stream, nil); !bytes.Equal(got, stream) {
		t.Errorf("no values at all still rewrote the stream:\n%s", got)
	}
}

// Every surface spells a text delta differently, and a fix that reached only
// the one it was found on would leave the other two carrying the same defect.
func TestCoalesceTextSpanningReachesEverySurface(t *testing.T) {
	for name, stream := range map[string]string{
		"anthropic.messages": `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"orch"}}` + "\n\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"estrator-5235a8fe"}}` + "\n\n",
		"openai.responses": `data: {"type":"response.output_text.delta","item_id":"m1","output_index":0,"delta":"orch"}` + "\n\n" +
			`data: {"type":"response.output_text.delta","item_id":"m1","output_index":0,"delta":"estrator-5235a8fe"}` + "\n\n",
		"openai.chat": `data: {"choices":[{"index":0,"delta":{"content":"orch"}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{"content":"estrator-5235a8fe"}}]}` + "\n\n",
	} {
		got := CoalesceTextSpanning([]byte(stream), []string{"orchestrator-5235a8fe"})
		if !bytes.Contains(got, []byte("orchestrator-5235a8fe")) {
			t.Errorf("%s: the value is still split:\n%s", name, got)
		}
	}
}
