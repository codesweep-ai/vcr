package cassette

import (
	"fmt"
	"path/filepath"
	"testing"
)

// script records n turns, each asking something different, and returns the
// store positioned at the start.
func script(t *testing.T, bodies ...string) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "script")
	s, err := OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range bodies {
		if _, err := s.Append(Recording{
			Entry:    Entry{Method: "POST", Path: "/v1/messages", Status: 200},
			Request:  []byte(b),
			Response: fmt.Appendf(nil, `{"n":%d}`, i+1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Reopen, so the test replays a cassette from disk rather than the one it
	// just wrote — which is the only state a replay session is ever in.
	reopened, err := OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	return reopened
}

func ask(body string) Request {
	return Request{Method: "POST", Path: "/v1/messages", Canonical: []byte(body)}
}

const (
	one   = `{"turn":1}`
	two   = `{"turn":2}`
	three = `{"turn":3}`
)

// The ordinary case: a session that makes the recorded requests in the recorded
// order gets the recorded answers, in order.
func TestAScriptIsServedInOrder(t *testing.T) {
	s := script(t, one, two, three)
	for i, body := range []string{one, two, three} {
		sel, miss := s.Next(ask(body), nil, 8, false)
		if miss != nil {
			t.Fatalf("step %d missed: %+v", i+1, miss)
		}
		if sel.Entry.Seq != i+1 || sel.Expected != i+1 {
			t.Errorf("served step %d expecting %d, want both %d", sel.Entry.Seq, sel.Expected, i+1)
		}
		if sel.Repeat {
			t.Errorf("step %d reported as a repeat", i+1)
		}
	}
}

// A client repeating itself is served the same answer again rather than
// consuming the next step. This is a retry, and it is Codex asking for the
// model list twice at startup — neither is the session moving on.
func TestARepeatedRequestIsServedTheSameStep(t *testing.T) {
	s := script(t, one, two)

	first, _ := s.Next(ask(one), nil, 8, false)
	again, miss := s.Next(ask(one), nil, 8, false)
	if miss != nil {
		t.Fatalf("a repeated request missed: %+v", miss)
	}
	if !again.Repeat || again.Entry.Seq != first.Entry.Seq {
		t.Errorf("repeat served step %d (repeat=%v), want step %d again", again.Entry.Seq, again.Repeat, first.Entry.Seq)
	}
	// And the session has not moved on: the next new request is still step 2.
	next, miss := s.Next(ask(two), nil, 8, false)
	if miss != nil || next.Entry.Seq != 2 {
		t.Errorf("after a repeat, next = %+v %+v, want step 2", next, miss)
	}
}

// Clients pipeline. Codex issues two `GET /models` at once and Claude Code runs
// title generation alongside its main loop, so a request can arrive before the
// one recorded ahead of it. Within the window that is served, and reported.
func TestARequestThatArrivesEarlyIsServedWithinTheWindow(t *testing.T) {
	s := script(t, one, two, three)

	sel, miss := s.Next(ask(three), nil, 8, false)
	if miss != nil {
		t.Fatalf("a pipelined request missed: %+v", miss)
	}
	if sel.Entry.Seq != 3 {
		t.Fatalf("served step %d, want 3", sel.Entry.Seq)
	}
	// Reported, so "the recorded order was reproduced" stays observable rather
	// than something the matching quietly gave up on.
	if sel.Expected != 1 {
		t.Errorf("expected = %d, want 1 — the step the session was at", sel.Expected)
	}
	// The skipped steps are still there, in order.
	for i, body := range []string{one, two} {
		got, miss := s.Next(ask(body), nil, 8, false)
		if miss != nil || got.Entry.Seq != i+1 {
			t.Errorf("step %d: %+v %+v", i+1, got, miss)
		}
	}
}

// The window is a bound on the search, not on similarity: too low costs a loud
// failure, never a quietly wrong answer.
func TestBeyondTheWindowIsAMiss(t *testing.T) {
	s := script(t, one, two, three)

	if sel, miss := s.Next(ask(three), nil, 1, false); miss == nil {
		t.Fatalf("step 3 was served with a window of 1: %+v", sel)
	} else if miss.Expected != 1 || miss.Entry == nil || miss.Entry.Seq != 1 {
		t.Errorf("miss = %+v, want it to name step 1", miss)
	}
	// Zero is strict: only the step the session is actually at.
	s2 := script(t, one, two)
	if _, miss := s2.Next(ask(two), nil, 0, false); miss == nil {
		t.Error("a strict window served a request out of order")
	}
	if _, miss := s2.Next(ask(one), nil, 0, false); miss != nil {
		t.Errorf("a strict window missed the request it was expecting: %+v", miss)
	}
}

// A request the recorded session never made is a miss whose message has to say
// what was expected instead — that is the whole difference between a fixable
// failure and an abandoned tool.
func TestARequestTheSessionNeverMadeMisses(t *testing.T) {
	s := script(t, one, two)
	sel, miss := s.Next(ask(`{"turn":"something else"}`), nil, 8, false)
	if miss == nil {
		t.Fatalf("an unrecorded request was served step %d", sel.Entry.Seq)
	}
	if miss.Entry == nil || miss.Entry.Seq != 1 {
		t.Fatalf("miss = %+v, want the step the session was at", miss)
	}
	if len(miss.Alignment.Leaf) == 0 && len(miss.Alignment.Shape) == 0 {
		t.Error("the miss carries no reason to report")
	}
}

// A script that has been played to the end reports that, rather than a diff
// against an entry that does not exist.
func TestAnExhaustedScriptSaysSo(t *testing.T) {
	s := script(t, one)
	if _, miss := s.Next(ask(one), nil, 8, false); miss != nil {
		t.Fatalf("step 1 missed: %+v", miss)
	}
	_, miss := s.Next(ask(two), nil, 8, false)
	if miss == nil {
		t.Fatal("a request past the end of the script was served")
	}
	if miss.Entry != nil {
		t.Errorf("miss names step %d, want none — the script ran out", miss.Entry.Seq)
	}
	if miss.Expected != 2 {
		t.Errorf("expected = %d, want 2", miss.Expected)
	}
}

// Method and path are checked as well as the body, because they are not in it:
// every agent's startup probes are bodiless, and two of them would otherwise
// align with each other trivially.
func TestABodilessRequestIsNotServedAnotherPathsEntry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "probes")
	s, err := OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/models", "/api/hello"} {
		if _, err := s.Append(Recording{
			Entry:   Entry{Method: "GET", Path: p, Status: 200},
			Request: nil, Response: []byte(`{"p":"` + p + `"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	s, err = OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}

	// The second probe, asked for first: same (empty) body, different path.
	sel, miss := s.Next(Request{Method: "GET", Path: "/api/hello"}, nil, 8, false)
	if miss != nil {
		t.Fatalf("a bodiless request missed: %+v", miss)
	}
	if sel.Entry.Path != "/api/hello" {
		t.Errorf("served %s, want the entry for the path asked for", sel.Entry.Path)
	}
}

// What the world answered may differ, and is carried out of the selection so a
// run can report it. The entry is still the recorded one.
func TestAToleratedDifferenceIsCarriedOutOfTheSelection(t *testing.T) {
	s := script(t, `{"turn":1,"observed":"a"}`)
	sel, miss := s.Next(ask(`{"turn":1,"observed":"b"}`), []Rule{"observed"}, 8, false)
	if miss != nil {
		t.Fatalf("a declared difference missed: %+v", miss)
	}
	if len(sel.Tolerated) != 1 || sel.Tolerated[0].Path != "observed" {
		t.Errorf("tolerated = %+v, want the one path named", sel.Tolerated)
	}
}

// A replay that stopped halfway is not a session that replayed, and the run has
// to be able to say which steps were never reached.
func TestRemainingNamesWhatWasNeverServed(t *testing.T) {
	s := script(t, one, two, three)
	s.Next(ask(one), nil, 8, false)

	left := s.Remaining()
	if len(left) != 2 || left[0].Seq != 2 || left[1].Seq != 3 {
		t.Errorf("remaining = %+v, want steps 2 and 3", left)
	}
}

// A miss reports against the step this request could have been, not against
// whatever the cursor happens to point at.
//
// Measured on a real session: a run made one startup probe where the recording
// made two, so the cursor sat on `GET /models` when the prompt arrived. The
// report said the prompt differed from a model-list request, which sends its
// reader nowhere — the useful sentence names the recorded turn and the one
// block of prompt that differs from it.
func TestAMissReportsAgainstTheStepItCouldHaveBeen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skipped")
	s, err := OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ method, path, body string }{
		{"GET", "/models", ""},
		{"GET", "/models", ""},
		{"POST", "/responses", `{"input":[{"text":"a"},{"text":"b"}]}`},
	} {
		if _, err := s.Append(Recording{
			Entry:   Entry{Method: e.method, Path: e.path, Status: 200},
			Request: []byte(e.body), Response: []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	s, err = OpenStore(dir, "test", 1, func() int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}

	// One probe where the recording made two, then a prompt one block short.
	if _, miss := s.Next(Request{Method: "GET", Path: "/models"}, nil, 8, false); miss != nil {
		t.Fatalf("the probe missed: %+v", miss)
	}
	_, miss := s.Next(Request{Method: "POST", Path: "/responses",
		Canonical: []byte(`{"input":[{"text":"a"}]}`)}, nil, 8, false)
	if miss == nil {
		t.Fatal("a prompt one block short of the recording was served")
	}
	if miss.Entry == nil || miss.Entry.Seq != 3 {
		t.Fatalf("miss names step %v, want the recorded /responses turn", miss.Entry)
	}
	if len(miss.Alignment.Shape) != 1 {
		t.Errorf("alignment = %+v, want the prompt difference explained", miss.Alignment)
	}
	// And the cursor is still where the session actually is.
	if miss.Expected != 2 {
		t.Errorf("expected = %d, want 2", miss.Expected)
	}
}

// A step the session never makes must not pin the window.
//
// A client may record a call it does not always make: Claude Code's title
// generation runs beside the main loop, and a session that recorded it can
// replay without it. If the window were measured from the oldest unclaimed
// step, that one step would bound the search for the rest of the session —
// every later request measured against something that will never be served.
// Measured on a real recording: 45 steps, of which the replay served 2.
func TestAStepTheSessionNeverMakesDoesNotPinTheWindow(t *testing.T) {
	bodies := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		bodies = append(bodies, fmt.Sprintf(`{"turn":%d}`, i))
	}
	s := script(t, bodies...)

	// Step 1 is the one this run does not make. Everything after it is served
	// in order, including steps far beyond a window anchored at step 1.
	for i := 2; i <= 12; i++ {
		got, miss := s.Next(ask(bodies[i-1]), nil, 2, false)
		if miss != nil {
			t.Fatalf("step %d missed with the session at step %d: %+v", i, i-1, miss)
		}
		if got.Entry.Seq != i {
			t.Fatalf("served step %d, want %d", got.Entry.Seq, i)
		}
	}
	// And the straggler is still findable, because the scan still starts at
	// the cursor rather than at the frontier.
	if got, miss := s.Next(ask(bodies[0]), nil, 2, false); miss != nil || got.Entry.Seq != 1 {
		t.Errorf("the skipped step was lost: %+v %+v", got, miss)
	}
}

// The window still bounds the search ahead of where the session has got.
func TestTheWindowStillBoundsTheSearchAhead(t *testing.T) {
	bodies := []string{`{"turn":1}`, `{"turn":2}`, `{"turn":3}`, `{"turn":4}`, `{"turn":5}`}
	s := script(t, bodies...)

	// Nothing served yet, so the frontier is the cursor: step 4 is three ahead
	// and a window of 2 must refuse it.
	if sel, miss := s.Next(ask(bodies[3]), nil, 2, false); miss == nil {
		t.Fatalf("step 4 was served with a window of 2 from a standing start: %+v", sel)
	}
}

// A probe repeated after the session has moved on is answered, not missed.
//
// Codex asks for the model list at startup and does not always ask the same
// number of times. When the extra ask arrives once the session is several
// steps in, the step that answers it is behind the last one served — and a
// miss on the model list fails a session as surely as a miss on a prompt.
func TestARepeatOfAnEarlierStepIsStillARepeat(t *testing.T) {
	probe := `{"probe":true}`
	s := script(t, probe, one, two)

	for _, want := range []int{1, 2, 3} {
		body := []string{probe, one, two}[want-1]
		if got, miss := s.Next(ask(body), nil, 8, false); miss != nil || got.Entry.Seq != want {
			t.Fatalf("step %d: %+v %+v", want, got, miss)
		}
	}
	// The session is at the end; the probe comes again.
	again, miss := s.Next(ask(probe), nil, 8, false)
	if miss != nil {
		t.Fatalf("a repeated startup probe missed: %+v", miss)
	}
	if !again.Repeat || again.Entry.Seq != 1 {
		t.Errorf("served step %d (repeat=%v), want step 1 again", again.Entry.Seq, again.Repeat)
	}
}

// Claude Code's title generation, which is what Config.AuxiliaryTurns exists
// for. The recording took it on haiku and the replay takes it on sonnet, so
// the two request keys share nothing: without the relaxation it misses as the
// session's first request, before anything has been served, and the campaign
// that measured this reported four such misses on a cassette recorded minutes
// earlier.
func TestAnAuxiliaryTurnIsMatchedOnShape(t *testing.T) {
	const (
		title = `{"messages":[{"text":"name this"}],"model":"claude-haiku-4-5","max_tokens":32000}`
		work  = `{"messages":[{"text":"do it"},{"text":"ok"}],"tools":[{"name":"bash"}],"model":"claude-sonnet-5"}`
		more  = `{"messages":[{"text":"do it"},{"text":"ok"},{"text":"next"}],"tools":[{"name":"bash"}],"model":"claude-sonnet-5"}`
		// The same call, on the model this run happened to pick.
		liveTitle = `{"messages":[{"text":"name this"}],"model":"claude-sonnet-5","max_tokens":64000}`
	)

	s := script(t, title, work, more)
	if _, miss := s.Next(ask(liveTitle), nil, 8, false); miss == nil {
		t.Fatal("without the relaxation the title turn must still miss, or this test proves nothing")
	}

	s = script(t, title, work, more)
	sel, miss := s.Next(ask(liveTitle), nil, 8, true)
	if miss != nil {
		t.Fatalf("the title turn missed: %+v", miss)
	}
	if !sel.Auxiliary || sel.Entry.Seq != 1 {
		t.Fatalf("served %+v, want the recorded title turn marked auxiliary", sel)
	}
	// The session's own work still has to align exactly.
	if _, miss := s.Next(ask(work), nil, 8, true); miss != nil {
		t.Fatalf("the real turn missed after the title turn was absorbed: %+v", miss)
	}
}

// A run that makes more auxiliary calls than the recording did must not stall:
// the count varies between runs the same way the model does.
func TestAnUnrecordedAuxiliaryTurnRepeatsTheLastOne(t *testing.T) {
	const (
		title = `{"messages":[{"text":"name this"}],"model":"claude-haiku-4-5"}`
		work  = `{"messages":[{"text":"a"},{"text":"b"}],"tools":[{"name":"bash"}],"model":"claude-sonnet-5"}`
		more  = `{"messages":[{"text":"a"},{"text":"c"}],"tools":[{"name":"bash"}],"model":"claude-sonnet-5"}`
	)
	s := script(t, title, work, more)
	if _, miss := s.Next(ask(title), nil, 8, true); miss != nil {
		t.Fatalf("the recorded title turn missed: %+v", miss)
	}
	sel, miss := s.Next(ask(`{"messages":[{"text":"and again"}]}`), nil, 8, true)
	if miss != nil {
		t.Fatalf("a second title turn the recording never made missed: %+v", miss)
	}
	if !sel.Repeat || !sel.Auxiliary {
		t.Fatalf("served %+v, want the recorded title answer repeated", sel)
	}
}

// A cassette recorded on ONE model still has its bookkeeping recognized.
//
// The model was the only signal at first, and it is the wrong one: a harness
// that wants a replayable cassette pins the model, and `claude -p --model X`
// puts the title generation on X as well. Every step then carries the same
// model, nothing is recognized as auxiliary, and the relaxation is off in the
// case it exists for. Measured on two recordings of one task, alike but for the
// credential: the half that let the client choose put its title call on haiku
// and replayed clean, and the half that pinned the model missed one request of
// three, on every run.
//
// What tells them apart here is the tool list. A step carrying twenty-one tools
// is the work, and a step carrying none is not.
func TestAnAuxiliaryTurnIsRecognizedOnAPinnedModel(t *testing.T) {
	const (
		pinned = "claude-fable-5"
		title  = `{"messages":[{"text":"name this"}],"model":"` + pinned + `"}`
		work   = `{"messages":[{"text":"do it"}],"tools":[{"name":"bash"}],"model":"` + pinned + `"}`
		// The same bookkeeping call, worded as this run happened to word it.
		liveTitle = `{"messages":[{"text":"name it differently"}],"model":"` + pinned + `"}`
	)
	s := script(t, title, work)
	sel, miss := s.Next(ask(liveTitle), nil, 8, true)
	if miss != nil {
		t.Fatalf("the title turn missed on a single-model cassette: %+v", miss)
	}
	if !sel.Auxiliary || sel.Entry.Seq != 1 {
		t.Fatalf("served %+v, want the recorded title turn marked auxiliary", sel)
	}
	// And the work still has to align exactly, on the same model.
	if _, miss := s.Next(ask(work), nil, 8, true); miss != nil {
		t.Fatalf("the real turn missed after the title turn was absorbed: %+v", miss)
	}
}

// A cassette with no work in it absorbs nothing, which is the guard that keeps
// the relaxation from swallowing ordinary traffic.
//
// Every step here is single-message, tool-free and on one model, so there is
// nothing for a bookkeeping call to be distinguished FROM. A client whose every
// request looks like this must still align exactly, or one of its turns would
// answer any other.
func TestASessionOfOnlyShapedTurnsAbsorbsNothing(t *testing.T) {
	const (
		one  = `{"messages":[{"text":"first"}],"model":"m"}`
		two  = `{"messages":[{"text":"second"}],"model":"m"}`
		live = `{"messages":[{"text":"neither"}],"model":"m"}`
	)
	s := script(t, one, two)
	if _, miss := s.Next(ask(live), nil, 8, true); miss == nil {
		t.Fatal("a cassette with no work in it absorbed an unrelated turn as bookkeeping")
	}
}

// The relaxation is for bookkeeping calls only. A single-message turn that
// carries tools is the session's work, and must still align exactly.
func TestWorkIsNotTreatedAsAuxiliary(t *testing.T) {
	const recorded = `{"messages":[{"text":"a"}],"tools":[{"name":"bash"}]}`
	const live = `{"messages":[{"text":"different"}],"tools":[{"name":"bash"}]}`
	s := script(t, recorded)
	if _, miss := s.Next(ask(live), nil, 8, true); miss == nil {
		t.Fatal("a turn carrying tools was absorbed as bookkeeping")
	}
}

// A miss has to name the step the request most nearly was. Every call to one
// provider shares a method and a path, so "the first unserved step that could
// have been this" is really just the oldest unclaimed step: the report then
// diffs the request against something nobody was trying to send, and the
// reader chases the wrong step. This is what made a campaign re-record a
// cassette that was never the problem.
func TestAMissNamesTheClosestStep(t *testing.T) {
	const (
		unrelated = `{"a":1,"b":2,"c":3,"d":4,"e":5}`
		near      = `{"turn":"work","detail":"recorded"}`
		live      = `{"turn":"work","detail":"live"}`
	)
	s := script(t, unrelated, near)
	_, miss := s.Next(ask(live), nil, 8, false)
	if miss == nil {
		t.Fatal("the request aligned with something, so this proves nothing")
	}
	if miss.Entry == nil || miss.Entry.Seq != 2 {
		t.Fatalf("miss names step %v, want step 2, the one it differs from least", miss.Entry)
	}
}

// The relaxation needs a recorded call that is visibly bookkeeping. A cassette
// whose steps are all one model has none, and a client that sends single-block
// prompts and no tools must still get a loud miss rather than any other step's
// answer. This is the case that made shape alone unusable as a default.
func TestAOneModelCassetteOffersNothingToAbsorbWith(t *testing.T) {
	const (
		first  = `{"input":[{"text":"a"}],"model":"gpt-5","seen":"x"}`
		second = `{"input":[{"text":"b"}],"model":"gpt-5","seen":"x"}`
		live   = `{"input":[{"text":"c"}],"model":"gpt-5","seen":"x"}`
	)
	s := script(t, first, second)
	if _, miss := s.Next(ask(live), nil, 8, true); miss == nil {
		t.Fatal("an ordinary single-block request was answered from another step")
	}
}
