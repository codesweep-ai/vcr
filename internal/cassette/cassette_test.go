package cassette

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func mustCreate(t *testing.T, dir string) *Cassette {
	t.Helper()
	c, err := Create(dir, "test", 1, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// the index is line-oriented so an entry can be appended while a
// response is still streaming. The property that matters is that an
// interrupted session leaves every completed entry readable, so the test
// interleaves appends and reads rather than writing everything then reading.
func TestAppendIsIncrementalAndSurvivesInterruption(t *testing.T) {
	dir := t.TempDir()
	c := mustCreate(t, dir)

	for i, hash := range []string{"aaa", "bbb", "ccc"} {
		if err := c.Append(Entry{Hash: hash, Surface: "anthropic.messages", Status: 200}); err != nil {
			t.Fatal(err)
		}
		got, err := c.Entries()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != i+1 {
			t.Fatalf("after %d appends, Entries() = %d", i+1, len(got))
		}
	}

	// A half-written final line is what an interrupted stream actually leaves
	// behind; the completed entries before it must still be readable.
	f, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"hash":"ddd"`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := c.Entries(); err == nil {
		t.Fatal("a truncated line was accepted as an entry")
	} else if !strings.Contains(err.Error(), "line 4") {
		t.Errorf("err = %v, want it to name the bad line", err)
	}
}

func TestEntriesOnMissingIndexIsEmptyNotAnError(t *testing.T) {
	c := mustCreate(t, t.TempDir())
	got, err := c.Entries()
	if err != nil {
		t.Fatalf("Entries() on a fresh cassette: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Entries() = %d, want 0", len(got))
	}
}

// Streaming decides which response file an entry refers to, so the two must
// never collide for one hash.
func TestResponsePathDistinguishesStreaming(t *testing.T) {
	c := mustCreate(t, t.TempDir())
	if a, b := c.ResponsePath(1, true), c.ResponsePath(1, false); a == b {
		t.Fatalf("streaming and non-streaming share a path: %s", a)
	}
	if !strings.HasSuffix(c.ResponsePath(1, true), ".sse") {
		t.Error("a streamed response should be a .sse file, so it diffs line by line")
	}
}

// List must not claim somebody else's directory: `prune` acts on what List
// returns, and a false positive there deletes data.
func TestListIgnoresDirectoriesThatAreNotCassettes(t *testing.T) {
	store := t.TempDir()
	mustCreate(t, filepath.Join(store, "real"))
	if err := os.MkdirAll(filepath.Join(store, "not-a-cassette"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "loose-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := List(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "real" {
		t.Fatalf("List = %v, want just [real]", got)
	}
}

func TestListOfMissingStoreIsEmptyNotAnError(t *testing.T) {
	got, err := List(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("List of a missing store: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

// Two version numbers, one verdict, one remedy.
//
// A format this build does not implement may parse into today's structs and
// mean something else; a different ruleset is another claim about which
// requests are equivalent. Both fail the same way — every entry parses, nothing
// errors, every request misses, and it reads as an agent that changed its mind
// — so both are refused at the record/replay door.
func TestACassetteFromAnotherBuildIsRefusedForRecordAndReplay(t *testing.T) {
	for _, c := range []struct {
		what  string
		meta  Meta
		names []string
	}{
		{"format", Meta{FormatVersion: 99, NormalizeVersion: 7}, []string{"format", "v99"}},
		{"ruleset", Meta{FormatVersion: FormatVersion, NormalizeVersion: 6}, []string{"ruleset", "v6"}},
	} {
		t.Run(c.what, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "stale")
			writeMeta(t, dir, c.meta)

			_, err := OpenStore(dir, "test", 7, func() int64 { return 0 })
			if err == nil {
				t.Fatalf("a cassette from another %s was opened for record/replay", c.what)
			}
			if !errors.Is(err, ErrStale) {
				t.Errorf("error = %v, want ErrStale", err)
			}
			// It has to say which number moved and what to do about it: this
			// error is the whole reason the versions are recorded.
			for _, want := range append(c.names, "record again", "verify") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// The negative half: this build's own cassette opens, and a stale one is left
// exactly as it was rather than replaced. Overwriting somebody's recordings
// because a version moved is not a recoverable mistake.
func TestARefusedCassetteIsNeitherOpenedNorReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keep")
	c, err := Create(dir, "test", 7, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Append(Entry{Hash: "abc", Method: "POST", Path: "/v1/messages", Status: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir, "test", 7, func() int64 { return 0 }); err != nil {
		t.Fatalf("this build's own cassette was refused: %v", err)
	}

	// Now the ruleset moves under it.
	if _, err := OpenStore(dir, "test", 8, func() int64 { return 0 }); err == nil {
		t.Fatal("a stale cassette was opened for record/replay")
	}
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("the refused cassette is no longer readable: %v", err)
	}
	es, err := got.Entries()
	if err != nil || len(es) != 1 || es[0].Hash != "abc" {
		t.Fatalf("entries = %v (%v), want the recording untouched", es, err)
	}
}

// Reading is separate from using: `verify` exists to report a version that
// moved, so a listing that refused to list would take the diagnosis away along
// with the failure.
func TestAStaleCassetteStillOpensForInspection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inspect")
	writeMeta(t, dir, Meta{FormatVersion: 1, NormalizeVersion: 6})

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("a stale cassette could not be inspected: %v", err)
	}
	if got.Meta.NormalizeVersion != 6 || got.Meta.FormatVersion != 1 {
		t.Errorf("meta = %+v, want the recorded versions reported as they are", got.Meta)
	}
	if err := got.Usable(7); err == nil {
		t.Error("Usable accepted a cassette two versions behind")
	}
}

// writeMeta lays down a cassette directory with exactly the given header.
func writeMeta(t *testing.T, dir string, m Meta) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "req"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cassette.yaml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
