package cassette

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
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
	// Kind is what a finding is reported as, and With is what stands in for the
	// value. Both have a default, `env:<Name>` and `<SECRET>`, which is what a
	// variable the caller named gets. The recorder's own identity brings its
	// own, so a username reads as `<USER>` in a diff.
	Kind string
	With string
}

// A literal shorter than minSecretLen is matched as a whole word, and one
// shorter than minWordLen is not looked for at all, which the report says.
//
// A short value inside ordinary prose is the risk: `ada` is in `adapter`, and a
// scrub that rewrites half the words in a prompt is worse than one that
// refuses. But a username is short, and it is the personal value most likely to
// be in a cassette, because a sandbox gives its guest the name of whoever
// launched it. On word boundaries it is found in `home/ada/app` and in
// `/tmp/ada-verify.js`, and `adapter` is left alone. Two characters are too few
// for even that.
const (
	minSecretLen = 12
	minWordLen   = 3
)

// protocolWords are short values that are not looked for even as whole words,
// because the API is written in them: every request of every cassette holds
// `"role":"user"`. A recorder whose login is one of these would have --force
// rewrite the protocol, and the finding count would say nothing. `root` and
// `localhost` are here for the same reason from another source: they are in
// every shell transcript and every dev-server address an agent prints.
var protocolWords = map[string]bool{
	"user": true, "system": true, "assistant": true, "developer": true, "tool": true,
	"root": true, "localhost": true,
}

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
//
// allowEmail names addresses that are not findings, each either a whole address
// or `@domain`, which covers that domain and everything under it.
func Scrub(dir string, secrets []Secret, allowEmail []string, apply bool) (ScrubReport, error) {
	var rep ScrubReport
	lits, skipped := literals(secrets)
	rep.Skipped = skipped
	keep := addressesToKeep(allowEmail)

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
		out, counts := scrubBytes(b, lits, keep)
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
		case len(s.Value) < minWordLen:
			skipped = append(skipped, Skipped{s.Name,
				fmt.Sprintf("under %d characters, so it would match ordinary text", minWordLen)})
		case protocolWords[strings.ToLower(s.Value)]:
			skipped = append(skipped, Skipped{s.Name,
				"it is a word the API itself uses, so every cassette holds it"})
		default:
			d := detector{kind: s.Kind, with: s.With, re: literal(s.Value)}
			if d.kind == "" {
				d.kind = "env:" + s.Name
			}
			if d.with == "" {
				d.with = "<SECRET>"
			}
			out = append(out, d)
		}
	}
	return out, skipped
}

// literal is the pattern a named value is looked for with: anywhere for a long
// one, and on word boundaries for a short one. A boundary is only asked for at
// an end that is a word character, because `\b` beside anything else never
// matches.
func literal(value string) *regexp.Regexp {
	pat := regexp.QuoteMeta(value)
	if len(value) < minSecretLen {
		if isWord(value[0]) {
			pat = `\b` + pat
		}
		if isWord(value[len(value)-1]) {
			pat += `\b`
		}
	}
	return regexp.MustCompile(pat)
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// reservedDomains belong to nobody. RFC 2606 sets aside the three example
// domains for documentation, and with RFC 6761 the four top-level names. An
// agent that needs an author for a commit invents `developer@example.com`, so
// reporting these fails the gate on every real session and protects no one.
var reservedDomains = []string{
	"example.com", "example.org", "example.net",
	"example", "invalid", "test", "localhost",
}

// addressesToKeep reports whether an address the detector matched is one that
// is not a finding: reserved, or allowed by the caller.
func addressesToKeep(allowEmail []string) func(match []byte) bool {
	var addresses, domains []string
	for _, a := range allowEmail {
		a = strings.ToLower(strings.TrimSpace(a))
		if rest, ok := strings.CutPrefix(a, "@"); ok {
			domains = append(domains, rest)
		} else if a != "" {
			addresses = append(addresses, a)
		}
	}
	domains = append(domains, reservedDomains...)
	return func(match []byte) bool {
		addr := strings.ToLower(string(match))
		if slices.Contains(addresses, addr) {
			return true
		}
		host := addr[strings.LastIndexByte(addr, '@')+1:]
		for _, d := range domains {
			// On a label boundary, so `myexample.com` is not `example.com`.
			if host == d || strings.HasSuffix(host, "."+d) {
				return true
			}
		}
		return false
	}
}

// scrubBytes applies the literals first and the shapes second, so a value the
// caller named is reported under its own name rather than under whichever
// pattern happens to match it.
func scrubBytes(b []byte, lits []detector, keepAddress func([]byte) bool) ([]byte, map[string]int) {
	counts := map[string]int{}
	for _, d := range append(slices.Clone(lits), detectors...) {
		n := 0
		out := d.re.ReplaceAllFunc(b, func(m []byte) []byte {
			if d.kind == "email" && keepAddress(m) {
				return m
			}
			n++
			return d.re.Expand(nil, []byte(d.with), m, d.re.FindSubmatchIndex(m))
		})
		if n == 0 {
			continue
		}
		counts[d.kind] += n
		b = out
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
