package cassette

import (
	"os"
	"sync"
	"time"
)

// Recording is everything one interaction needs, apart from the metadata that
// goes in the index.
type Recording struct {
	Entry Entry
	// Request is the canonical request body — the exact bytes that were
	// hashed, so the file a reviewer reads is the thing that was matched.
	Request []byte
	// Response is the response body, or the raw SSE stream for a streamed one.
	Response []byte
}

// Store is a cassette open for reading and writing: the ordered script, and a
// cursor into it.
//
// A cassette is a SCRIPT, not a set. Replay reproduces the recorded session in
// the recorded order, so an entry is selected by position and then verified by
// alignment — see SPEC.md section 5. Hashing the whole body to find an
// entry made every non-semantic byte of a 60 KB prompt load-bearing, and the
// ruleset that had to anticipate them all went through four versions in a week.
type Store struct {
	c *Cassette

	mu     sync.Mutex
	script []Entry
	// served marks the entries already handed out, so a look-ahead skips them
	// and the cursor can advance past a run of them.
	served []bool
	// cursor is the position replay expects next: the earliest unserved entry.
	cursor int
	// last is the entry most recently served, which a client's own retry — and
	// Codex's duplicate startup probe — is served again rather than advancing.
	last int
	// canon caches request bodies read for alignment, so a look-ahead over the
	// same few entries does not re-read them per request.
	canon map[int][]byte
}

// OpenStore opens a cassette for record/replay, creating it if absent. This is
// the door that checks the versions; Open, which the inspection commands use,
// does not.
func OpenStore(dir, proxyVersion string, normalizeVersion int, now func() int64) (*Store, error) {
	c, err := OpenOrCreate(dir, proxyVersion, normalizeVersion, unixTime(now()))
	if err != nil {
		return nil, err
	}
	if err := c.Usable(normalizeVersion); err != nil {
		return nil, err
	}
	entries, err := c.Entries()
	if err != nil {
		return nil, err
	}
	return &Store{
		c:      c,
		script: entries,
		served: make([]bool, len(entries)),
		last:   -1,
		canon:  map[int][]byte{},
	}, nil
}

// Cassette exposes the underlying cassette, for the commands that inspect it.
func (s *Store) Cassette() *Cassette { return s.c }

// Len reports how many interactions the script holds.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.script)
}

// Request is the live request being matched against the script.
type Request struct {
	Method, Path string
	// Canonical is the normalized body, which is what alignment compares.
	Canonical []byte
}

// Selection is the entry replay will serve.
type Selection struct {
	Entry Entry
	// Expected is where the cursor stood. Equal to Entry.Seq in a session that
	// reproduced the recorded order exactly, which is the point of saying it.
	Expected int
	// Repeat marks an entry served a second time: a client retrying, or Codex
	// asking for the model list twice at startup.
	Repeat bool
	// Tolerated is what differed under a volatile path — the world answering
	// differently. Carried out so a run can report it and correlation can use it.
	Tolerated []Difference
}

// Miss is why nothing in the window could be served.
type Miss struct {
	Expected int
	// Entry is the one at the cursor, absent if the script ran out.
	Entry *Entry
	// Alignment is that entry's, which is the diff worth showing: the request
	// the session was supposed to make next, against the one it made.
	Alignment Alignment
	// Err is set when the two could not be compared at all — a body that will
	// not parse. Reported as itself: an empty alignment is not agreement, and
	// saying "the bodies align" here sent a reader looking at the method.
	Err error
}

// Next selects the entry to serve for a live request.
//
// The earliest unserved entry that aligns, within a window. A larger window
// cannot serve a wrong entry — alignment is exact, so an entry that aligns IS
// this request — which is what makes a bound acceptable here at all: it is a
// search bound, not a similarity threshold, and setting it too low costs a loud
// failure rather than a quietly wrong answer.
//
// The window exists because clients pipeline. Codex issues two identical
// `GET /models` at startup, and Claude Code runs title generation in parallel
// with its main loop; both would fail a strictly positional match through no
// fault of the session.
func (s *Store) Next(req Request, volatile []Rule, lookahead int) (*Selection, *Miss) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// candidate is the first unserved step in the window that this request
	// could plausibly have been — same method, same path. It is what a miss
	// reports against, because the cursor is often something else entirely: a
	// session that skipped a startup probe would otherwise be told its prompt
	// differs from `GET /models`, which sends its reader nowhere.
	candidate := -1
	var candidateAl Alignment

	seen := 0
	for i := s.cursor; i < len(s.script); i++ {
		if s.served[i] {
			continue
		}
		if seen > lookahead {
			break
		}
		seen++
		al, ok := s.aligns(i, req, volatile)
		if !ok {
			if candidate < 0 && s.script[i].Method == req.Method && s.script[i].Path == req.Path {
				candidate, candidateAl = i, al
			}
			continue
		}
		sel := &Selection{Entry: s.script[i], Expected: s.cursor + 1, Tolerated: al.Tolerated}
		s.served[i] = true
		s.last = i
		for s.cursor < len(s.script) && s.served[s.cursor] {
			s.cursor++
		}
		return sel, nil
	}

	// Nothing new fits. A request identical to the one just served is the
	// client repeating itself, and repeating the answer is what it expects —
	// this is a retry, or a probe a client makes twice.
	if s.last >= 0 {
		if al, ok := s.aligns(s.last, req, volatile); ok {
			return &Selection{Entry: s.script[s.last], Expected: s.cursor + 1,
				Repeat: true, Tolerated: al.Tolerated}, nil
		}
	}

	miss := &Miss{Expected: s.cursor + 1}
	// The step this request could have been, if there is one; otherwise the
	// step the session was at, which at least says what was expected instead.
	at := candidate
	if at < 0 {
		at = s.cursor
	}
	if at < len(s.script) {
		e := s.script[at]
		miss.Entry = &e
		if candidate >= 0 {
			miss.Alignment = candidateAl
			return nil, miss
		}
		if canon, err := s.canonical(at); err == nil {
			var alErr error
			miss.Alignment, alErr = Align(canon, req.Canonical, volatile)
			if alErr != nil {
				miss.Err = alErr
			}
		}
	}
	return nil, miss
}

// aligns reports whether the entry at i is this request: the same method and
// path, and a body that aligns. Method and path are checked because they are
// not in the body — two bodiless probes to different endpoints would otherwise
// align with each other trivially.
func (s *Store) aligns(i int, req Request, volatile []Rule) (Alignment, bool) {
	e := s.script[i]
	if e.Method != req.Method || e.Path != req.Path {
		return Alignment{}, false
	}
	canon, err := s.canonical(i)
	if err != nil {
		return Alignment{}, false
	}
	al, err := Align(canon, req.Canonical, volatile)
	if err != nil {
		return Alignment{}, false
	}
	return al, al.Matches()
}

func (s *Store) canonical(i int) ([]byte, error) {
	if b, ok := s.canon[i]; ok {
		return b, nil
	}
	b, err := os.ReadFile(s.c.RequestPath(s.script[i].Seq))
	if err != nil {
		return nil, err
	}
	s.canon[i] = b
	return b, nil
}

// Remaining lists the entries never served, which is what a replay session owes
// its reader: a script that stopped halfway is not a session that replayed.
func (s *Store) Remaining() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for i, e := range s.script {
		if !s.served[i] {
			out = append(out, e)
		}
	}
	return out
}

// Response reads a recorded response body.
func (s *Store) Response(e Entry) ([]byte, error) {
	return os.ReadFile(s.c.ResponsePath(e.Seq, e.Streaming))
}

// Append writes an interaction to the end of the script: the canonical request,
// the response, and the index line — in that order, so a crash mid-write leaves
// an unreferenced body file rather than an index entry pointing at nothing. An
// unreferenced file is what `cassette prune` is for; a dangling index entry
// would fail a replay.
//
// The sequence number is assigned here rather than by the caller: it is the
// store that knows how long the script is, and a caller that guessed could
// overwrite a turn.
func (s *Store) Append(r Recording) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r.Entry.Seq = len(s.script) + 1
	if err := os.WriteFile(s.c.RequestPath(r.Entry.Seq), r.Request, 0o644); err != nil {
		return Entry{}, err
	}
	if err := os.WriteFile(s.c.ResponsePath(r.Entry.Seq, r.Entry.Streaming), r.Response, 0o644); err != nil {
		return Entry{}, err
	}
	if err := s.c.Append(r.Entry); err != nil {
		return Entry{}, err
	}
	s.script = append(s.script, r.Entry)
	s.served = append(s.served, true)
	s.canon[len(s.script)-1] = r.Request
	s.cursor = len(s.script)
	s.last = len(s.script) - 1
	return r.Entry, nil
}

// unixTime converts a unix second count to a time, keeping the time package out
// of the Store's signature so callers can inject a clock in tests.
func unixTime(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
