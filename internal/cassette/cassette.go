// Package cassette is the on-disk recording format.
//
// This is the durable asset. The proxy around it could be rewritten
// in a weekend; a format with bad normalization semantics or no streaming model
// gets lived with for years. So it is specified independently of the proxy,
// versioned, and shaped for the two things it is actually for: being read in a
// PR diff, and being appended to while a response is still streaming.
//
// A cassette is a DIRECTORY, not a file:
//
//	cassettes/refactor-auth/
//	  cassette.yaml     format version, ruleset version, provenance
//	  index.jsonl       one line per step, in the order they happened
//	  req/0001.json     the normalized request, pretty-printed
//	  resp/0001.json    a non-streaming response body
//	  resp/0001.sse     a streamed response, one event per line
//
// A single JSON file per entry would put a 40 KB system prompt and 200 SSE
// events on one line, so a prompt change shows up in review as one unreadable
// +/- pair; splitting bodies into their own files makes the same change a
// handful of legible line hunks. The index stays line-oriented so an entry can
// be appended without rewriting the file.
//
// Bodies are addressed by POSITION, not by content hash. A cassette is a
// script: two turns of one session can be the same request with different
// answers — a client retries, or asks the same thing twice — and content
// addressing gives those one file, so the second answer overwrites the first.
// Numbering also makes `ls resp/` read as the session.
package cassette

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FormatVersion is everything about a cassette that this BUILD decides: the
// layout on disk, and the canonical form a request is reduced to before it is
// compared. Bump it when either changes.
//
// One number rather than two, because the two failed identically and were fixed
// identically. A cassette from another version is refused rather than read
// optimistically — every entry would still parse and every request would miss,
// which is indistinguishable from an agent that changed its mind.
//
// The user's ruleset has a version of its own, in `normalize.version`, because
// it is configuration and this build cannot know what it says.
const FormatVersion = 3

// Meta is cassette.yaml — everything true of the cassette as a whole.
type Meta struct {
	FormatVersion int    `yaml:"format_version" json:"format_version"`
	Created       string `yaml:"created" json:"created"`
	// ProxyVersion and NormalizeVersion are provenance: which build
	// wrote this, and under which claim about request equivalence. Without the
	// second, `cassette verify` cannot tell a stale entry from a changed rule.
	ProxyVersion     string `yaml:"proxy_version" json:"proxy_version"`
	NormalizeVersion int    `yaml:"normalize_version" json:"normalize_version"`
}

// Entry is one recorded interaction: one line of index.jsonl.
//
// Everything needed to MATCH is here; the bodies live in their own files, so
// selecting an entry never means parsing a megabyte of prompt.
type Entry struct {
	// Seq is this entry's position in the script, from 1. It is what selects
	// an entry: a cassette is an ordered recording of a session, and replay
	// reproduces that order rather than searching for a body that hashes the
	// same. It also names the body files, so `ls resp/` reads as the session.
	Seq int `json:"seq"`
	// Hash identifies the request, for logs and for `cassette show`. It no
	// longer selects anything.
	Hash string `json:"hash"`
	// Method and Path are part of the key, and are kept here so `cassette show`
	// can say what an entry was without re-deriving it.
	Method   string `json:"method"`
	Path     string `json:"path"`
	Provider string `json:"provider"`
	Surface  string `json:"surface"`
	Model    string `json:"model,omitempty"`
	Status   int    `json:"status"`
	// Streaming decides which response file to read: <hash>.sse or <hash>.json.
	Streaming bool `json:"streaming"`
	// ContentType is replayed verbatim, because a client picks its parser from
	// it and a guessed one turns a stream back into a blob.
	ContentType string `json:"content_type,omitempty"`
	// RetryAfter is the recorded `Retry-After`, and the only response header
	// besides the content type a cassette keeps.
	//
	// Kept only for a status a client retries, because that is the only case
	// where a client reads it, and only in delay-seconds form. RFC 7231 also
	// allows an HTTP-date, which is a timestamp belonging to the recording run,
	// and that is the class of header this format drops.
	//
	// A recorded rate limit is replayed (see proxy.isTransient), and a client
	// that waited three seconds while recording waits three seconds again. Drop
	// the header and it falls back to its own default instead, which replays a
	// session nobody recorded.
	RetryAfter string `json:"retry_after,omitempty"`
	// The rest of the response's headers are not here, and that is the whole
	// list: see proxy.serveFromCassette for why a `Set-Cookie` and a `Date` from
	// the recording run are worth less than nothing to the client replaying.
	RecordedAt string `json:"recorded_at"`
	LatencyMS  int64  `json:"latency_ms"`
}

// Cassette is an open cassette directory.
type Cassette struct {
	Dir  string
	Meta Meta
}

// ErrStale is returned for a cassette whose versions are not this build's.
// There is one error for all three because there is one remedy: re-record.
var ErrStale = errors.New("cassette was recorded by a different build")

// Open reads an existing cassette, whatever versions it carries.
//
// Reading is separate from using. `cassette ls`, `show` and `verify` open a
// stale cassette on purpose — verify exists to report exactly that, and a
// listing that refuses to list because the ruleset moved takes the diagnosis
// away along with the failure. Record and replay go through OpenStore, which
// calls Usable.
func Open(dir string) (*Cassette, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cassette.yaml"))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	return &Cassette{Dir: dir, Meta: m}, nil
}

// Usable reports whether this build can record into and replay from the
// cassette: two version numbers, one verdict.
//
// A format this build does not implement may parse into today's structs and
// mean something else; a different ruleset is another claim about which
// requests are equivalent. Either way each entry still parses, nothing reports
// an error, and every request misses — indistinguishable from an agent that
// changed its mind, the one thing a replay failure must never be confused with.
// And either way the remedy is to record it again.
//
// Recording is refused as well as replaying, because entries written now would
// be keyed under this build's rules and sit beside entries keyed under the old
// ones, in a cassette whose header can only claim one of them.
func (c *Cassette) Usable(normalizeVersion int) error {
	var was, now int
	var what string
	switch {
	case c.Meta.FormatVersion != FormatVersion:
		what, was, now = "format", c.Meta.FormatVersion, FormatVersion
	case c.Meta.NormalizeVersion != normalizeVersion:
		what, was, now = "normalization ruleset", c.Meta.NormalizeVersion, normalizeVersion
	default:
		return nil
	}
	return fmt.Errorf("%w: %s has %s v%d, this build uses v%d — its keys mean something else now, "+
		"so every request would miss; delete it and record again "+
		"(`cs-vcr cassette verify` shows what changed)", ErrStale, c.Dir, what, was, now)
}

// Create makes a new cassette directory.
func Create(dir, proxyVersion string, normalizeVersion int, now time.Time) (*Cassette, error) {
	for _, sub := range []string{"req", "resp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	m := Meta{
		FormatVersion:    FormatVersion,
		Created:          now.UTC().Format(time.RFC3339),
		ProxyVersion:     proxyVersion,
		NormalizeVersion: normalizeVersion,
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "cassette.yaml"), b, 0o644); err != nil {
		return nil, err
	}
	return &Cassette{Dir: dir, Meta: m}, nil
}

// OpenOrCreate opens a cassette, creating it if absent. An existing cassette
// in an unreadable state is an error, never a silent replacement: overwriting
// somebody's recordings because their format was one version ahead is not a
// recoverable mistake.
func OpenOrCreate(dir, proxyVersion string, normalizeVersion int, now time.Time) (*Cassette, error) {
	_, err := os.Stat(filepath.Join(dir, "cassette.yaml"))
	switch {
	case err == nil:
		return Open(dir)
	case os.IsNotExist(err):
		return Create(dir, proxyVersion, normalizeVersion, now)
	default:
		return nil, err
	}
}

// Entries reads the index.
func (c *Cassette) Entries() ([]Entry, error) {
	f, err := os.Open(c.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Entry
	sc := bufio.NewScanner(f)
	// A single entry's index line is small, but a long tool schema list in a
	// future field should not truncate the scan into silent data loss.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", c.indexPath(), n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Append adds an entry to the index. Opening in append mode per call is what
// makes a cassette safe to write while a stream is still arriving: the index
// is never rewritten, so an interrupted session leaves every completed entry
// intact.
func (c *Cassette) Append(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(c.indexPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (c *Cassette) indexPath() string { return filepath.Join(c.Dir, "index.jsonl") }

// RequestPath and ResponsePath name an entry's body files, by position.
//
// By position rather than by content hash, because two turns of one session can
// be the same request with different answers — a client retries, or asks the
// same thing twice — and content addressing gives them one file, so the second
// answer overwrites the first. A script has to hold both.
func (c *Cassette) RequestPath(seq int) string {
	return filepath.Join(c.Dir, "req", seqName(seq)+".json")
}

func (c *Cassette) ResponsePath(seq int, streaming bool) string {
	ext := ".json"
	if streaming {
		ext = ".sse"
	}
	return filepath.Join(c.Dir, "resp", seqName(seq)+ext)
}

// seqName zero-pads so a directory listing sorts into session order.
func seqName(seq int) string { return fmt.Sprintf("%04d", seq) }

// List returns the cassette names under a store directory, sorted.
func List(store string) ([]string, error) {
	ents, err := os.ReadDir(store)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		// A directory without a cassette.yaml is somebody else's directory,
		// not a broken cassette; listing it would invite `prune` to eat it.
		if _, err := os.Stat(filepath.Join(store, e.Name(), "cassette.yaml")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}
