package cassette

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Scrubbing is what makes a private recording publishable.
//
// It is not part of the request path, and nothing about it weakens the rule
// that cs-vcr holds no credential and redacts nothing while proxying: request
// headers are never recorded, so the credential an agent authenticated with is
// not in a cassette to begin with. What IS in one is whatever the session put in
// a body — a key a prompt quoted, a token a tool call carried, the address the
// agent was told its user has — and a cassette is committed.
//
// So this is a separate, explicit pass over a cassette on disk, run by a person
// before the commit, reporting what it found and changing nothing unless asked.
//
// A scrub that changes a REQUEST changes what replay matches on. That is a
// feature and the reason findings are reported one file at a time: a value that
// had to be taken out of a request was going to make the cassette replay for
// nobody but the person who recorded it. The remedy is a `normalize` rule, which
// blanks the value on both sides, and the scrub is what tells you one is needed.

// Secret is a value the caller knows is sensitive, matched literally.
//
// Named rather than anonymous so a report can say which one was found without
// printing it. Values arrive from the environment, never from the command line,
// where every process on the machine can read them.
type Secret struct {
	Name  string
	Value string
}

// A literal shorter than this is not scrubbed, and the report says so. A
// four-character value matches inside ordinary prose, and a scrub that rewrites
// half the words in a prompt is worse than one that refuses.
const minSecretLen = 12

// detector is one shape of credential or personal data, and what replaces it.
type detector struct {
	kind string
	re   *regexp.Regexp
	with string
}

// The shapes worth naming: the credentials the three providers cs-vcr fronts
// issue, the two forms any of them can be carried in, and the one piece of
// personal data an agent reliably puts in a prompt.
//
// Deliberately short. A detector that fires on ordinary text costs more than the
// case it covers, because it rewrites a prompt that was matching, and `--from-env`
// is the exact answer for a value this list does not know.
var detectors = []detector{
	// The leading group is a word boundary Go's RE2 cannot express as one: a
	// key starts where a run of key characters starts, and `sk-` in the middle
	// of one is not a key. Without it every base64 blob in a cassette is a
	// finding — an encrypted-reasoning field matched 1018 characters because
	// `sk-` happened to fall inside it, and a scrub that cries wolf on ordinary
	// recordings is one nobody can gate on.
	{"anthropic-key", regexp.MustCompile(`(^|[^A-Za-z0-9_-])sk-ant-[A-Za-z0-9_-]{16,}`), "${1}<API-KEY>"},
	{"openai-key", regexp.MustCompile(`(^|[^A-Za-z0-9_-])sk-(?:proj-)?[A-Za-z0-9_-]{20,}`), "${1}<API-KEY>"},
	{"fireworks-key", regexp.MustCompile(`(^|[^A-Za-z0-9_-])fw_[A-Za-z0-9]{20,}`), "${1}<API-KEY>"},
	{"github-token", regexp.MustCompile(`(^|[^A-Za-z0-9_-])gh[pousr]_[A-Za-z0-9]{30,}`), "${1}<API-KEY>"},
	// A JWT is an access token in the shape every OAuth provider mints it, and
	// its middle segment is a readable account record.
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "<JWT>"},
	{"bearer-token", regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{20,}`), "Bearer <TOKEN>"},
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "<PRIVATE-KEY>"},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,24}`), "<EMAIL>"},
	// An account handle rather than a credential: it authenticates nobody, and
	// the provider that issued it can still resolve it to the account that
	// recorded the cassette. It travels in responses, so replacing it changes
	// nothing a request is matched on.
	{"account-id", regexp.MustCompile(`"(?:safety_identifier|user)"\s*:\s*"user-[A-Za-z0-9]{16,}"`), `"safety_identifier":"<ACCOUNT>"`},
}

// Finding is one kind of value found in one file of a cassette, and how many
// times. The value itself is never carried: a report is printed, logged and
// pasted into issues, and a scrubber that quotes what it found puts the secret
// somewhere new every time it runs.
type Finding struct {
	File  string
	Kind  string
	Count int
}

// Skipped is a secret that was named but could not be looked for.
type Skipped struct {
	Name string
	Why  string
}

// ScrubReport is what one pass over a cassette found and, when asked, changed.
type ScrubReport struct {
	Findings []Finding
	Skipped  []Skipped
	// Rewritten is how many files were changed. Zero when only reporting.
	Rewritten int
}

// Total is how many values were found, across every kind and file.
func (r ScrubReport) Total() int {
	n := 0
	for _, f := range r.Findings {
		n += f.Count
	}
	return n
}

// Scrub finds credentials and personal data in a cassette, and replaces them
// with placeholders when apply is set.
//
// Every file in the directory is scanned, index and metadata included: a path
// recorded in the index is as readable as one in a body.
func Scrub(dir string, secrets []Secret, apply bool) (ScrubReport, error) {
	var rep ScrubReport
	lits, skipped := literals(secrets)
	rep.Skipped = skipped

	found := map[string]map[string]int{} // file -> kind -> count
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = path
		}
		out, counts := scrubBytes(b, lits)
		if len(counts) == 0 {
			return nil
		}
		found[rel] = counts
		if !apply {
			return nil
		}
		// Written with the mode the recorder uses, so a scrubbed cassette is
		// indistinguishable from a freshly recorded one.
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return err
		}
		rep.Rewritten++
		return nil
	})
	if err != nil {
		return rep, err
	}
	rep.Findings = flatten(found)
	return rep, nil
}

// literals turns named secrets into the ones worth searching for, and reports
// the rest rather than dropping them quietly: a caller that named a variable
// expects it to have been looked for.
func literals(secrets []Secret) ([]detector, []Skipped) {
	var out []detector
	var skipped []Skipped
	for _, s := range secrets {
		switch {
		case s.Value == "":
			skipped = append(skipped, Skipped{s.Name, "not set in this environment"})
		case len(s.Value) < minSecretLen:
			skipped = append(skipped, Skipped{s.Name,
				fmt.Sprintf("under %d characters, so it would match ordinary text", minSecretLen)})
		default:
			out = append(out, detector{
				kind: "env:" + s.Name,
				re:   regexp.MustCompile(regexp.QuoteMeta(s.Value)),
				with: "<SECRET>",
			})
		}
	}
	return out, skipped
}

// scrubBytes applies the literals first and the shapes second, so a value the
// caller named is reported under its own name rather than under whichever
// pattern happens to match it.
func scrubBytes(b []byte, lits []detector) ([]byte, map[string]int) {
	counts := map[string]int{}
	for _, d := range append(append([]detector{}, lits...), detectors...) {
		n := len(d.re.FindAll(b, -1))
		if n == 0 {
			continue
		}
		counts[d.kind] += n
		b = d.re.ReplaceAll(b, []byte(d.with))
	}
	return b, counts
}

// flatten orders the findings by file and then by kind, so two runs over one
// cassette print the same report.
func flatten(found map[string]map[string]int) []Finding {
	var out []Finding
	for file, kinds := range found {
		for kind, n := range kinds {
			out = append(out, Finding{File: file, Kind: kind, Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
