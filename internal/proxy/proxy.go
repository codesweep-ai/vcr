// Package proxy is the origin-mode listener: the agent addresses cs-vcr
// directly over plain HTTP (ANTHROPIC_BASE_URL / OPENAI_BASE_URL), and cs-vcr
// makes the real call upstream.
//
// It records and replays. It does not authenticate callers, validate tokens,
// swap credentials or redact anything: whatever a client sends is forwarded
// untouched, so an agent keeps whatever login it already had — an API key, or
// a Claude Pro/Max subscription that has no token anyone else could substitute.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
)

// Server is the origin-mode HTTP handler.
type Server struct {
	cfg *config.Config
	log *slog.Logger
	// cassette is what this session records into and replays from, used by any
	// request not attributed to a client that names its own.
	cassette *cassette.Store
	// cassettes is the one per client label, for the campaign case: several
	// agents through one cs-vcr, each with its own recording. Sharing one would
	// share a key namespace, and two agents given the same opening prompt
	// normalize to the same request — see Config.checkCassettesAreDistinct.
	cassettes map[string]*cassette.Store
	// offline is set by the command that built this server, and nothing else
	// can change it: `replay` constructs an offline server, `record` an online
	// one. Reaching a provider is then a property of which binary invocation you
	// made rather than of a flag, a file, or an environment variable — none of which
	// are present at the moment someone is deciding whether to trust it.
	offline bool
	// now is injectable so a test can assert on a recorded timestamp.
	now func() time.Time
	// missDir, when set, is where a missed request's normalized body is
	// written. The reply already carries a diff against the nearest entry, but
	// nearest is measured against a body that is mostly a shared system prompt,
	// so it can point at an unrelated turn. The request itself is the thing to
	// diff, and by the time anyone wants it the agent that sent it is gone.
	//
	// Off unless asked for: replay reads a cassette, and a session that writes
	// to disk by default is one that dirties a checkout it was handed.
	missDir string

	mu    sync.Mutex
	stats Stats
}

// Stats is the session accounting behind the shutdown summary, which is what a
// CI log shows and what decides the exit code.
type Stats struct {
	Requests int `json:"requests"`
	// Unmatched counts requests whose path belonged to no configured client —
	// almost always a base URL missing its prefix, which is worth its own
	// counter because it looks like silence rather than like an error.
	Unmatched int `json:"unmatched"`
	Upstream  int `json:"upstream"`
	// Replayed and Recorded are the two numbers that say whether a session did
	// what it was asked to. Misses is the third: in replay mode it is the one
	// that fails the build.
	Replayed int `json:"replayed"`
	Recorded int `json:"recorded"`
	Misses   int `json:"misses"`
	Rejected int `json:"rejected"`
	// OutOfOrder counts entries served at a position other than the one the
	// script expected — a client that pipelined, almost always. Reported so
	// that "the recorded order was reproduced" stays an observable property.
	OutOfOrder int `json:"out_of_order"`
	// Drifted counts differences accepted under a volatile path: the world
	// answering differently than it did when the cassette was recorded.
	Drifted int `json:"drifted"`
	// InFlight counts the requests still being answered. It is worth reporting
	// at the END, where it is no longer a request in flight but an interaction
	// the session walked out on: the entry is written once the response is done,
	// so each one left here is a provider call whose answer nothing kept.
	InFlight  int            `json:"in_flight"`
	BySurface map[string]int `json:"by_surface"`
	ByLabel   map[string]int `json:"by_label"`
}

// New builds a server. The logger is required: every rejection path logs, and
// a nil logger would make the quiet failures the hardest ones to diagnose.
//
// offline is a constructor argument rather than an optional chained method
// because it is the safety property. An optional method that grants safety is
// one a caller can omit silently, and the result still compiles and still
// reports itself as offline while dialing the provider. A parameter cannot be
// forgotten: leaving it out does not build.
func New(cfg *config.Config, log *slog.Logger, offline bool) *Server {
	return &Server{
		cfg:     cfg,
		log:     log,
		offline: offline,
		now:     time.Now,
		stats:   Stats{BySurface: map[string]int{}, ByLabel: map[string]int{}},
	}
}

// WithCassette attaches the cassette this session records into and replays from.
func (s *Server) WithCassette(t *cassette.Store) *Server { s.cassette = t; return s }

// WithClientCassette attaches the cassette a labelled client records into.
func (s *Server) WithClientCassette(label string, t *cassette.Store) *Server {
	if s.cassettes == nil {
		s.cassettes = map[string]*cassette.Store{}
	}
	s.cassettes[label] = t
	return s
}

// cassetteFor is the cassette a request from this client belongs in.
//
// Callers name the result `store`, after its type, rather than `cassette`:
// this package calls into the cassette package throughout, and a local of that
// name would shadow it. The domain word is used everywhere it can be.
func (s *Server) cassetteFor(label string) *cassette.Store {
	if t, ok := s.cassettes[label]; ok {
		return t
	}
	return s.cassette
}

// WithMissDump writes each missed request's normalized body into dir, named by
// its hash, so it can be diffed against the cassette's own req/<hash>.json.
func (s *Server) WithMissDump(dir string) *Server { s.missDir = dir; return s }

// ReachesUpstream reports whether this server may open a connection to a
// provider. Read in one place, so a rule enforced once cannot be forgotten
// twice.
func (s *Server) ReachesUpstream() bool { return !s.offline }

// Snapshot returns a copy of the session counters.
func (s *Server) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.stats
	cp.BySurface = copyCounts(s.stats.BySurface)
	cp.ByLabel = copyCounts(s.stats.ByLabel)
	return cp
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	maps.Copy(out, m)
	return out
}

func (s *Server) count(f func(*Stats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.stats)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// WHO first, and from the connection: a path prefix on the base URL. It
	// costs a client nothing but its base URL, and works for a caller that has
	// no token to be identified by.
	client, rest, matched := s.cfg.MatchClient(r.URL.Path)
	if !matched {
		s.count(func(st *Stats) { st.Requests++; st.Unmatched++ })
		s.log.Warn("rejected: no client matches this path",
			slog.String("path", r.URL.Path), slog.String("remote", r.RemoteAddr))
		writeError(w, http.StatusNotFound, "unknown_client",
			"no client is configured for this path; expected a base URL ending in one of: "+strings.Join(s.cfg.Prefixes(), ", "))
		return
	}
	// Upstream must see the provider's own path, not cs-vcr's addressing, and
	// the strip has to happen before anything else reads the path or every
	// request would classify as an unrecognized surface.
	stripClientPrefix(r, rest)

	label := "default"
	if client != nil {
		label = client.Label
	}
	route := classify(r, s.cfg.DefaultProvider)
	// A client that names its provider has already answered the question, for
	// every path it sends: the prefix is a base URL, and a client configures one
	// base URL per provider. Nothing about the request itself can override that.
	if client != nil && client.Provider != "" {
		route.Provider = client.Provider
	}
	s.count(func(st *Stats) {
		st.Requests++
		st.BySurface[string(route.Surface)]++
		st.ByLabel[label]++
	})

	if !route.Surface.Recordable() {
		// an unrecognized path is either a client cs-vcr has not been
		// taught or a surface that changed under it, and both want to be
		// visible. It is still recorded and still replayed — a request that is
		// proxied but not recorded is one replay can never serve, and a real
		// Claude Code run diverged for exactly that reason: its /api/hello
		// probe succeeded while recording and errored while replaying.
		s.log.Debug("unrecognized path: proxying and recording anyway",
			slog.String("path", r.URL.Path), slog.String("label", label))
	}

	prov, err := s.provider(route.Provider)
	if err != nil {
		s.log.Error("no upstream configured", slog.String("provider", route.Provider))
		writeError(w, http.StatusBadGateway, "no_provider", err.Error())
		return
	}

	// The request body is read here and put back, because it is needed twice:
	// once to compute the key, and once to forward. Buffering it whole is
	// affordable — a request body is a prompt, and the streaming that matters
	// is on the response.
	body, err := readAndRestoreBody(r)
	if err != nil {
		s.log.Error("could not read request body", slog.Any("err", err))
		writeError(w, http.StatusBadRequest, "unreadable_body", "could not read the request body")
		return
	}
	// The target, not just the path: the query selects provider behaviour, so
	// two requests differing only in it are two interactions.
	key := cassette.Normalize(r.Method, requestTarget(r), body, &s.cfg.Normalize)

	store := s.cassetteFor(label)

	// Replay takes the next entry of the script, and there is no code path from
	// here to a socket: the safety is structural rather than a branch further
	// down.
	//
	// Recording consults nothing. A cassette is the ordered record of a session,
	// so a recording run makes every call the session makes, including the ones
	// it makes twice — serving one from the cassette would leave a script with a
	// turn missing from the middle of it.
	if !s.ReachesUpstream() {
		if store == nil {
			// A miss like any other: there is nothing to serve, and the client
			// needs the same answer whether the cassette is empty or absent.
			s.count(func(st *Stats) { st.Rejected++; st.Misses++ })
			s.reportMiss(w, nil, &cassette.Miss{Expected: 1}, key, label, r.URL.Path)
			return
		}
		sel, miss := store.Next(cassette.Request{
			Method: r.Method, Path: key.Target, Canonical: key.Canonical,
		}, cassette.Rules(s.cfg.Normalize.VolatilePaths()), s.cfg.Lookahead)
		if miss != nil {
			s.count(func(st *Stats) { st.Rejected++; st.Misses++ })
			// key.Target, not r.URL.Path: the entry records a target, so a
			// report that holds a bare path beside it calls every query
			// parameter a changed endpoint. A body that stopped matching on
			// /v1/messages?beta=true then reads as "this run asked for
			// /v1/messages instead", and the reader goes looking for a routing
			// fault that is not there.
			s.reportMiss(w, store, miss, key, label, key.Target)
			return
		}
		s.serveFromCassette(w, store, sel, label, key.Captured)
		return
	}

	// Counted around the call, not before it, so that what is left standing at
	// shutdown is exactly the set of interactions nobody waited for.
	s.count(func(st *Stats) { st.Upstream++; st.InFlight++ })
	defer s.count(func(st *Stats) { st.InFlight-- })
	s.forward(w, r, prov, recordCtx{
		on: store != nil, store: store, key: key, route: route, label: label,
		// key.Target, not the target as it arrived: the index records what the
		// entry is keyed on, so a stripped query parameter is absent from both
		// or from neither.
		method: r.Method, path: key.Target,
	})
}

// readAndRestoreBody reads the body and leaves the request able to be forwarded.
func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	// ContentLength is already correct; setting GetBody lets the transport
	// retry an idempotent request without the body having been consumed.
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	return b, nil
}

// serveFromCassette replays a recorded interaction.
//
// It reproduces two response headers: the content type, and `Retry-After` on a
// step whose status a client retries. The cassette holds no others.
//
// A successful response carries no `anthropic-ratelimit-*` to put back at all.
// What it does carry is `Set-Cookie`, `Date` and a Cloudflare request id — a
// recording run's session cookie and a stale timestamp. Replaying those causes
// misses rather than preventing them, and storing them would put a live
// credential in a committed cassette, which is the one thing "no redaction"
// depends on never happening.
//
// A rate limit is the case those reasons do not cover. It is recorded as a step
// like any other, so it replays, and the client's next move is the wait the
// header names.
func (s *Server) serveFromCassette(w http.ResponseWriter, store *cassette.Store, sel *cassette.Selection, label string, captured map[string]string) {
	entry := sel.Entry
	body, err := store.Response(entry)
	if err == nil {
		// Put this run's paths and identifiers back: the client is about to
		// act on them, and a recorded value names a file that exists only on
		// the machine that recorded it.
		body = s.cfg.Normalize.RestoreResponse(body, captured)
	}
	if err != nil {
		s.count(func(st *Stats) { st.Rejected++ })
		s.log.Error("cassette entry has no response on disk",
			slog.String("hash", entry.Hash), slog.Any("err", err))
		writeError(w, http.StatusInternalServerError, "cassette_corrupt",
			"the cassette index references a response that is not on disk; run `cs-vcr cassette verify`")
		return
	}
	s.count(func(st *Stats) { st.Replayed++ })
	s.log.Info("replayed", slog.Int("seq", entry.Seq),
		slog.String("label", label), slog.Bool("streaming", entry.Streaming))

	// The session was supposed to reproduce the recorded order. Where it did
	// not, say so: clients pipeline, so this is usually benign, and a run that
	// never mentioned it would make "the sequence was reproduced exactly" an
	// unobservable claim rather than a property.
	if entry.Seq != sel.Expected && !sel.Repeat {
		s.count(func(st *Stats) { st.OutOfOrder++ })
		s.log.Warn("served out of recorded order",
			slog.Int("expected", sel.Expected), slog.Int("served", entry.Seq),
			slog.String("label", label))
	}
	// What differed under a volatile path is the world answering differently.
	// Accepted, never silent: it is how an environment that moved under a
	// cassette becomes visible instead of becoming a wrong answer.
	for _, d := range sel.Tolerated {
		s.count(func(st *Stats) { st.Drifted++ })
		s.log.Warn("tolerated a changed observation",
			slog.Int("seq", entry.Seq), slog.String("path", d.Path),
			slog.String("recorded", cassette.Short(d.Recorded)), slog.String("live", cassette.Short(d.Live)))
	}

	// The recorded Content-Type, verbatim: a client picks its parser from it,
	// and guessing one is how a replayed stream stops looking like a stream.
	ct := entry.ContentType
	if ct == "" {
		ct = "application/json"
		if entry.Streaming {
			ct = "text/event-stream"
		}
	}
	w.Header().Set("Content-Type", ct)
	// Recorded only on a status a client retries, so this puts the recorded
	// wait back where the client is about to ask for one.
	if entry.RetryAfter != "" {
		w.Header().Set("Retry-After", entry.RetryAfter)
	}
	if entry.Streaming {
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(entry.Status)
		replayStream(w, body)
		return
	}
	w.WriteHeader(entry.Status)
	_, _ = w.Write(body)
}

// reportMiss answers a replay-mode miss with something a human can act on.
//
// It exists because "cassette miss" with no further information is the
// single most likely cause of this tool being abandoned. The reply names the
// nearest recorded request and shows how it differs, so the usual cause — a
// prompt that changed by one line — is visible without opening the cassette.
func (s *Server) reportMiss(w http.ResponseWriter, store *cassette.Store, miss *cassette.Miss, key cassette.Key, label, target string) {
	// The request is named in the reply, not only in the log. An agent shows its
	// user the message body and nothing else, so a miss that says only that it
	// missed sends whoever is watching to go and find the server's log — which,
	// in a sandbox, is on the other side of a network namespace.
	name := s.cfg.Cassette
	if store != nil {
		name = filepath.Base(store.Cassette().Dir)
	}
	msg := fmt.Sprintf("no recording for %s at step %d of cassette %q (client %s), and `replay` never contacts a provider",
		target, miss.Expected, name, label)
	detail := explain(miss, target)
	s.log.Error("cassette miss",
		slog.Int("expected", miss.Expected), slog.String("label", label),
		slog.String("target", target), slog.String("why", detail))
	s.dumpMiss(miss, key, target)
	// 400, and the status is chosen by what the client does with it rather than
	// by what it means. It must not be retryable: Stainless SDKs retry a 5xx,
	// which turns two misses into sixteen requests and a run that hangs to its
	// timeout instead of failing in one round trip. It must also not be a
	// status the SDK reinterprets: 404 on /v1/messages is how the API reports
	// an unknown model, so a miss reaches the operator as "that model may not
	// exist or you may not have access to it" and sends them to their model
	// config.
	//
	// 400 is neither retried nor renamed, and the client prints the message
	// below, which says what actually happened.
	writeError(w, http.StatusBadRequest, "cassette_miss", msg+detail)
}

// dumpMiss writes the request that missed, in the same normalized form the
// cassette stores, and NAMES IT AFTER THE STEP it was compared against — so
// `diff cassettes/x/req/0003.json misses/0003.json` answers the question
// directly, and `cs-vcr calibrate` can pair the two without guessing.
//
// A request that matched no step at all cannot be paired, and says so in its
// name rather than borrowing a number that would pair it with the wrong turn.
//
// A failure to write is logged and no more: the miss is already being reported,
// and losing its copy must not change what the client is told.
func (s *Server) dumpMiss(miss *cassette.Miss, key cassette.Key, target string) {
	if s.missDir == "" {
		return
	}
	if err := os.MkdirAll(s.missDir, 0o755); err != nil {
		s.log.Error("could not create the miss dump directory", slog.Any("err", err))
		return
	}
	name := "unpaired-" + key.Hash[:12]
	if miss != nil && miss.Entry != nil {
		name = fmt.Sprintf("%04d", miss.Entry.Seq)
	}
	file := filepath.Join(s.missDir, name+".json")
	if err := os.WriteFile(file, key.Canonical, 0o644); err != nil {
		s.log.Error("could not dump the missed request", slog.Any("err", err))
		return
	}
	s.log.Info("missed request written", slog.String("file", file), slog.String("target", target))
}

// explain turns an alignment into the sentence a miss has to be, which is the
// difference between a fixable failure and an abandoned tool.
//
// It names the step the session was at, the request that was expected there,
// and the paths that disagreed — not a line diff of two 60 KB bodies, which is
// what the old nearest-match report degenerated into once a prompt was mostly
// shared boilerplate.
func explain(miss *cassette.Miss, target string) string {
	if miss.Entry == nil {
		return fmt.Sprintf("\nthe cassette has %d steps and they have all been served; "+
			"this run made a request the recorded session did not", miss.Expected-1)
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "\nstep %d was recorded as %s %s (%s)",
		miss.Entry.Seq, miss.Entry.Method, miss.Entry.Path, miss.Entry.Surface)
	// Both sides are targets — path and query, after strip_query — because the
	// entry holds one and a comparison against anything else answers a question
	// nobody asked.
	if miss.Entry.Method != "" && (miss.Entry.Path != target) {
		fmt.Fprintf(b, "\nthis run asked for %s instead", target)
		return b.String()
	}
	if miss.Err != nil {
		fmt.Fprintf(b, "\nthe two could not be compared: %v", miss.Err)
		return b.String()
	}
	al := miss.Alignment
	if len(al.Shape) == 0 && len(al.Leaf) == 0 {
		fmt.Fprint(b, "\nthe bodies align — the method or path differs")
		return b.String()
	}
	// Shape first: an added or removed item explains every value difference
	// beneath it, so reporting the values first would bury the cause.
	for _, d := range al.Shape {
		fmt.Fprintf(b, "\n  %s: %s", d.Path, d.Why)
	}
	for _, d := range al.Leaf {
		fmt.Fprintf(b, "\n  %s\n    recorded: %s\n    this run: %s",
			d.Path, cassette.Short(d.Recorded), cassette.Short(d.Live))
	}
	// The advice is only offered for a VALUE that differs. A shape difference
	// means the request is built differently — an item added or gone — and
	// declaring that path volatile would excuse the whole list it is in, which
	// for a prompt is the prompt. There is no rule for it, and suggesting one
	// would send a reader to blank the thing they are trying to match on.
	if len(al.Shape) == 0 && len(al.Leaf) > 0 {
		fmt.Fprintf(b, "\n\nif %q is something the world decides rather than the agent, "+
			"declare it under normalize.volatile", cassette.Generalize(al.Leaf[0].Path))
	}
	return b.String()
}

func (s *Server) provider(name string) (*config.Provider, error) {
	p, ok := s.cfg.Providers[name]
	if !ok {
		return nil, fmt.Errorf("no upstream configured for provider %q", name)
	}
	return p, nil
}

// recordCtx is what forward needs to know to record what passes through it.
type recordCtx struct {
	on           bool
	store        *cassette.Store
	key          cassette.Key
	route        Route
	label        string
	method, path string
}

// forward makes the upstream call. Headers cross unchanged — including whatever
// credential the client authenticated with, which is the only thing that can
// work for a subscription login and which cs-vcr has no reason to touch.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, prov *config.Provider, rec recordCtx) {
	base, err := url.Parse(prov.BaseURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_base_url", "provider base_url is not a URL")
		return
	}
	// The tap sits between the proxy and the client, so the recording is the
	// bytes the client actually received rather than a re-encoding of them.
	out := w
	var t *tap
	if rec.on {
		t = &tap{ResponseWriter: w}
		out = t
	}
	started := s.now()

	// The recording is DEFERRED, because the call at the bottom of this function
	// does not always return.
	//
	// A client that has what it needs hangs up, and by then the response is
	// already streaming — too late for the ReverseProxy to report an error to
	// anyone, so it abandons the request the only way left to it: it panics with
	// http.ErrAbortHandler. net/http recovers that one silently, by design, and
	// everything written after ServeHTTP is therefore skipped. The request had
	// reached the provider, the tap was holding its bytes, and nothing was
	// written, logged or counted: the only trace was a summary reading
	// `upstream calls 2 / recorded 1`, which is how this was found.
	//
	// A deferred call runs while that panic unwinds, and does not stop it.
	complete := false
	if t != nil {
		defer func() {
			// net/http cancels the request context when the connection goes
			// away, which is what tells a client that left from a response
			// upstream cut short.
			s.record(t, rec, started, complete, r.Context().Err() != nil)
		}()
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			// SetURL preserves the inbound Host, which upstream TLS vhosting
			// rejects; the request must claim the host it is actually going to.
			pr.Out.Host = base.Host

			// Drop the client's Accept-Encoding so Go's transport negotiates
			// its own and transparently decompresses. Without this the recorder
			// captures whatever the client asked for — and Claude Code asks for
			// gzip, so cassettes came out as `1f 8b …` binary blobs: unreadable
			// in a PR diff, which is the whole reason the format exists, and
			// unreplayable, because the recorded Content-Encoding was lost and
			// the client fell back to non-streaming when the bytes would not
			// parse.
			//
			// The cost is that a recording session transfers uncompressed
			// bodies. That is the right trade for a tool whose output is meant
			// to be read.
			pr.Out.Header.Del("Accept-Encoding")
		},
		// -1 flushes each write straight through, which is what an SSE stream
		// needs: the default buffering turns a token-by-token stream into
		// arriving-in-lumps, and a recorder that sees lumps records lumps.
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(s.log.Handler(), slog.LevelError),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.log.Error("upstream failed", slog.Any("err", err))
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		},
	}
	rp.ServeHTTP(out, r)
	complete = true
}

// record writes what the tap captured into the cassette.
//
// It runs from a defer, so it has to be correct on every way out of forward,
// including the panic a ReverseProxy raises when a response is abandoned
// mid-copy. complete says the copy reached its own end; clientGone says the
// connection went away under it.
func (s *Server) record(t *tap, rec recordCtx, started time.Time, complete, clientGone bool) {
	if t.status == 0 {
		// Nothing ever reached the client — not even an error from cs-vcr's own
		// handler — so there is no response to record. Said out loud all the
		// same: a request that went upstream and left no entry is exactly the
		// silence this deferred write exists to end.
		s.log.Warn("upstream produced no response; nothing to record",
			slog.String("hash", rec.key.Hash[:12]), slog.String("label", rec.label))
		return
	}
	if !complete {
		// Recorded anyway, and as it stands. A cassette holds the bytes the
		// client received, and these are all of the ones it received — which for
		// the common case is the whole interaction, because a client hangs up
		// once it has read the last event it wanted.
		//
		// Warned about because the other case looks identical from here: an
		// answer cut off halfway replays as an answer cut off halfway, and
		// nothing else in the run would say so.
		reason := "the response ended before upstream finished it"
		if clientGone {
			reason = "the client closed the connection before the response ended"
		}
		s.log.Warn("recording an interrupted response: "+reason,
			slog.String("hash", rec.key.Hash[:12]), slog.String("label", rec.label),
			slog.Int("events", t.events), slog.Int("bytes", t.body.Len()))
	}
	if t.dropped > 0 {
		// The client received the whole response and the cassette holds the
		// first maxBody of it. Recorded anyway: a step missing from the middle
		// shifts every later one, so dropping it would replay as a session that
		// diverged rather than as a recording that is short. Warned about
		// because half an answer replays as half an answer, and a client waiting
		// for the end of a stream that never arrives hangs to its own timeout.
		s.log.Warn("the response outgrew the capture limit; this step replays truncated, so re-record it",
			slog.String("hash", rec.key.Hash[:12]), slog.String("label", rec.label),
			slog.Int("kept", t.body.Len()), slog.Int("dropped", t.dropped))
	}
	// Record everything except what a client would retry.
	//
	// The line is transient versus deterministic, not success versus failure. A
	// 429 or a 503 is a fact about one moment upstream, and freezing it into a
	// cassette turns a passing blip into a permanent failure. A 404 is a fact
	// about the REQUEST, and will be a 404 every time. Refusing to record it
	// breaks the run: Claude Code's startup probe 404s, and an unrecorded 404
	// misses on replay and fails the build for a request whose real answer was
	// always that 404.
	if isTransient(t.status) {
		// Recorded as a step like any other, and worth saying out loud: a
		// cassette carrying a rate limit will replay that rate limit, and the
		// client will retry it exactly as it did — which is faithful, and is
		// also a sign the recording was made against a provider having a bad
		// moment. Re-record if that is not the session you meant to keep.
		s.log.Warn("recording a transient failure as a step of the session",
			slog.Int("status", t.status), slog.String("label", rec.label))
	}
	entry := cassette.Entry{
		Hash:        rec.key.Hash,
		Method:      rec.method,
		Path:        rec.path,
		Provider:    rec.route.Provider,
		Surface:     string(rec.route.Surface),
		Model:       modelOf(rec.key.Canonical),
		Status:      t.status,
		Streaming:   t.sse,
		ContentType: t.Header().Get("Content-Type"),
		RetryAfter:  retryAfter(t.Header(), t.status),
		RecordedAt:  s.now().UTC().Format(time.RFC3339),
		LatencyMS:   s.now().Sub(started).Milliseconds(),
	}
	written, err := rec.store.Append(cassette.Recording{
		Entry:   entry,
		Request: rec.key.Canonical,
		// Coalesce first, then normalize. A model streams tool arguments as
		// fragments split at arbitrary character boundaries, so a path or an
		// id inside them is not contiguous and nothing can substitute it;
		// joining the fragments is what makes the value reachable. Then the
		// response gets the same treatment as the request, because a client
		// echoes a recorded tool argument straight back into its next one.
		Response: s.cfg.Normalize.ApplyResponse(cassette.CoalesceToolInput(t.body.Bytes()), rec.key.Captured),
	})
	if err != nil {
		// A failed write is not worth failing the client's request over: the
		// interaction succeeded, and losing the recording is recoverable by
		// running the session again.
		s.log.Error("could not write the cassette entry", slog.Any("err", err))
		return
	}
	s.count(func(st *Stats) { st.Recorded++ })
	s.log.Info("recorded", slog.Int("seq", written.Seq),
		slog.String("label", rec.label), slog.String("model", entry.Model),
		slog.Bool("streaming", entry.Streaming), slog.Int("events", t.events))
}

// retryAfter is the `Retry-After` a cassette keeps from a response.
//
// Only for a status a client retries, because that is the only case where a
// client reads the header at all. A successful response that carries one is
// telling the caller about a quota it is not currently blocked by, and freezing
// that into a cassette dates it.
//
// And only in delay-seconds form. RFC 7231 also allows an HTTP-date, which is a
// moment in the recording run: replay it and the deadline it names has always
// already passed, so a client computes a negative wait and retries at once —
// the opposite of the backoff the recording captured.
func retryAfter(h http.Header, status int) string {
	if !isTransient(status) {
		return ""
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" || strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return ""
	}
	return v
}

// isTransient reports whether a status is one a client would retry. The set
// matches what Stainless-generated SDKs retry, which is what the clients cs-vcr
// records use. It decides what to warn about, not what to record: a script
// holds the session as it happened, retries included.
func isTransient(status int) bool {
	return status == http.StatusRequestTimeout || // 408
		status == http.StatusConflict || // 409
		status == http.StatusTooManyRequests || // 429
		status >= 500
}

// modelOf pulls the model out of a canonical request body, for the index line.
// Metadata, so a body that does not name one is not an error.
func modelOf(canonical []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(canonical, &v)
	return v.Model
}

// requestTarget is the path with its query, which is what identifies an
// interaction to a provider.
func requestTarget(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return r.URL.Path
	}
	return r.URL.Path + "?" + r.URL.RawQuery
}

// stripClientPrefix rewrites the request path to what upstream should see,
// after the client's prefix has identified it.
//
// RawPath matters: it is set only when the escaped form differs from Path, and
// leaving a stale one behind would send upstream a path that disagrees with
// the one just matched.
func stripClientPrefix(r *http.Request, rest string) {
	if r.URL.Path == rest {
		return
	}
	r.URL.Path = rest
	r.URL.RawPath = ""
}

// errorBody is the shape of every error cs-vcr returns. It is structured
// because a client parses it and a human reads it in a CI log, and it never
// carries request content.
type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Source  string `json:"source"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	var b errorBody
	b.Error.Type = kind
	b.Error.Message = msg
	b.Error.Source = "cs-vcr"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}
