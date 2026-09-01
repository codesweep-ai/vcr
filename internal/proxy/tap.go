package proxy

import (
	"bytes"
	"net/http"
	"strings"
)

// maxBody bounds the copy a tap keeps, and is a var so a test can lower it.
//
// The copy is unbounded work on an unbounded input. A response streams for as
// long as the model keeps talking, and nothing in HTTP obliges it to stop: a
// very long agent turn, a stream that never terminates, or an upstream that has
// started repeating itself all grow this buffer for the life of the request.
// What follows is an out-of-memory kill in the middle of a recording session,
// which is neither reported nor recoverable — the least actionable failure this
// process has.
//
// 64 MiB sits far above any real answer, because a long streamed turn is a few
// hundred kilobytes. It bounds the pathological case, per response in flight.
var maxBody = 64 << 20

// tap is the ResponseWriter the recorder hands to the upstream proxy: it
// forwards everything to the real client and keeps a copy.
//
// The copy is taken on the way through rather than by re-reading afterwards,
// because a streamed response has no afterwards — it is consumed as it arrives,
// and a recorder that waited for the end would have to buffer the whole stream
// before the client saw any of it, turning a token-by-token response into a
// long pause.
type tap struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	// events counts SSE event terminators seen, which is the number worth
	// reporting: "recorded 214 events" says something, "recorded 47 KB" does not.
	// Counted over the copy rather than over the traffic, so it describes the
	// recording that was made and not the response that went past it.
	events int
	sse    bool
	// dropped is what the copy would not hold, in bytes. The client received it
	// all; only the cassette is short, and by this much.
	dropped int
	// wroteHeader guards against the double WriteHeader that a proxy error
	// handler can otherwise trigger after upstream has already replied.
	wroteHeader bool
	// selfGenerated says the status here is cs-vcr's own rather than a
	// provider's: the reverse proxy's error handler wrote it, and upstream
	// contributed nothing. Set only where that write actually took the header,
	// so an error arriving after a real reply leaves this false.
	selfGenerated bool
}

func (t *tap) WriteHeader(code int) {
	if t.wroteHeader {
		return
	}
	t.wroteHeader = true
	t.status = code
	t.sse = strings.HasPrefix(t.Header().Get("Content-Type"), "text/event-stream")
	t.ResponseWriter.WriteHeader(code)
}

func (t *tap) Write(p []byte) (int, error) {
	if !t.wroteHeader {
		t.WriteHeader(http.StatusOK)
	}
	if t.body.Len() == 0 {
		t.sniff(p)
	}
	// Only the copy is bounded. Every byte still reaches the client, because a
	// recorder that truncated the response it was observing would change the
	// session it exists to reproduce.
	kept := p
	if room := maxBody - t.body.Len(); len(kept) > room {
		if room < 0 {
			room = 0
		}
		kept = kept[:room]
		t.dropped += len(p) - room
	}
	t.body.Write(kept)
	if t.sse {
		t.events += bytes.Count(kept, []byte("\n\n"))
	}
	return t.ResponseWriter.Write(p)
}

// sniff decides from the first bytes whether a response with no Content-Type is
// a stream.
//
// An upstream is not obliged to label one, and one that matters does not: the
// Codex backend at chatgpt.com sends event frames from the first byte under no
// content type at all. Believing the missing header recorded that stream as
// `<hash>.json` with `streaming: false` and no framing, and replayed it as a
// single blob to a client waiting for events.
//
// Only where the header is absent. A stated content type is the upstream's own
// claim about its body and outranks a guess from two bytes — including the
// reverse case, a `text/event-stream` that arrives empty.
func (t *tap) sniff(p []byte) {
	if t.sse || t.Header().Get("Content-Type") != "" {
		return
	}
	// The two frame fields a stream can open with. A JSON body cannot begin
	// with either — it begins with `{` or `[` — so this cannot mistake one for
	// a stream.
	t.sse = bytes.HasPrefix(p, []byte("event:")) || bytes.HasPrefix(p, []byte("data:"))
}

// Flush passes the flush through. Without it the ReverseProxy's immediate-flush
// behaviour is lost the moment the writer is wrapped, and an SSE stream is
// delivered in one lump at the end — which is the exact failure the recorder
// exists to avoid reproducing.
func (t *tap) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// replayStream writes a recorded SSE stream back, one event at a time, flushing
// after each.
//
// It reproduces EVENT boundaries, not the original TCP chunk boundaries. That
// is deliberate: no HTTP client API exposes where a TCP write landed, so chunk
// boundaries are not observable to the thing being tested, while event
// boundaries are exactly what an incrementally-assembling client reacts to.
// Replaying per event is therefore both more faithful to what a client can see
// and more deterministic than reproducing arbitrary splits.
func replayStream(w http.ResponseWriter, stream []byte) {
	flusher, _ := w.(http.Flusher)
	for len(stream) > 0 {
		i := bytes.Index(stream, []byte("\n\n"))
		if i < 0 {
			// A trailing fragment: the recording was cut short, so replay what
			// there is rather than dropping it. A truncated stream is a real
			// thing to reproduce — it is how a cancelled request looks.
			_, _ = w.Write(stream)
			break
		}
		event := stream[:i+2]
		if _, err := w.Write(event); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		stream = stream[i+2:]
	}
	if flusher != nil {
		flusher.Flush()
	}
}
