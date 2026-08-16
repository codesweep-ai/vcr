package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/spf13/cobra"
)

func newCassetteCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cassette",
		Short: "Inspect and maintain cassettes",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newCassetteLsCmd(app), newCassetteShowCmd(app), newCassetteVerifyCmd(app),
		newCassetteScrubCmd(app), newCassettePruneCmd(app))
	return cmd
}

func newCassetteLsCmd(app *App) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls [cassette]",
		Short: "List cassettes, or the entries in one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := app.Cfg.Cassettes
			if len(args) == 0 {
				return listCassettes(cmd.OutOrStdout(), store, asJSON)
			}
			return listEntries(cmd.OutOrStdout(), filepath.Join(store, args[0]), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print stable machine-readable JSON")
	return cmd
}

func listCassettes(out io.Writer, store string, asJSON bool) error {
	names, err := cassette.List(store)
	if err != nil {
		return err
	}
	type row struct {
		Name    string `json:"name"`
		Entries int    `json:"entries"`
		Created string `json:"created"`
	}
	rows := make([]row, 0, len(names))
	for _, n := range names {
		c, err := cassette.Open(filepath.Join(store, n))
		if err != nil {
			return err
		}
		es, err := c.Entries()
		if err != nil {
			return err
		}
		rows = append(rows, row{Name: n, Entries: len(es), Created: c.Meta.Created})
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintf(out, "no cassettes in %s\n", store)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENTRIES\tCREATED")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", r.Name, r.Entries, r.Created)
	}
	return tw.Flush()
}

func listEntries(out io.Writer, dir string, asJSON bool) error {
	c, err := cassette.Open(dir)
	if err != nil {
		return err
	}
	es, err := c.Entries()
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(es)
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tMETHOD\tPATH\tSURFACE\tMODEL\tSTATUS\tSTREAM")
	for _, e := range es {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t%s\n",
			e.Seq, e.Method, e.Path, e.Surface, orDash(e.Model), e.Status, yn(e.Streaming))
	}
	return tw.Flush()
}

func newCassetteShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <cassette> <step|hash>",
		Short: "Print one entry's request and response",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := filepath.Join(app.Cfg.Cassettes, args[0])
			c, err := cassette.Open(dir)
			if err != nil {
				return err
			}
			es, err := c.Entries()
			if err != nil {
				return err
			}
			for _, e := range es {
				// A step number first, because that is what `ls` leads with and
				// what a cassette is ordered by. A hash prefix still works, for
				// a reader coming from a log line.
				if strconv.Itoa(e.Seq) == args[1] {
					return showEntry(cmd.OutOrStdout(), c, e)
				}
				if e.Hash == args[1] || short(e.Hash) == args[1] ||
					len(args[1]) >= 6 && len(e.Hash) >= len(args[1]) && e.Hash[:len(args[1])] == args[1] {
					return showEntry(cmd.OutOrStdout(), c, e)
				}
			}
			return fmt.Errorf("no step or hash matching %q in %s", args[1], dir)
		},
	}
}

func showEntry(out io.Writer, c *cassette.Cassette, e cassette.Entry) error {
	fmt.Fprintf(out, "step      %d\n", e.Seq)
	fmt.Fprintf(out, "hash      %s\n", e.Hash)
	fmt.Fprintf(out, "surface   %s (%s)\n", e.Surface, e.Provider)
	fmt.Fprintf(out, "model     %s\n", orDash(e.Model))
	fmt.Fprintf(out, "status    %d\n", e.Status)
	fmt.Fprintf(out, "recorded  %s (%dms upstream)\n", e.RecordedAt, e.LatencyMS)
	for _, f := range []struct {
		label, path string
	}{
		{"request", c.RequestPath(e.Seq)},
		{"response", c.ResponsePath(e.Seq, e.Streaming)},
	} {
		b, err := os.ReadFile(f.path)
		if err != nil {
			fmt.Fprintf(out, "\n--- %s (%s): %v\n", f.label, f.path, err)
			continue
		}
		fmt.Fprintf(out, "\n--- %s (%s)\n%s", f.label, f.path, b)
	}
	return nil
}

func newCassetteVerifyCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "verify [cassette...]",
		Short: "Check cassettes against the current normalization ruleset",
		Long: `Check every entry against the ruleset this build would apply now, and report
the ones that would no longer match. Contacts no provider, so it is safe as a
pre-merge gate.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			names := args
			if len(names) == 0 {
				var err error
				if names, err = cassette.List(app.Cfg.Cassettes); err != nil {
					return err
				}
			}
			return verifyCassettes(cmd.OutOrStdout(), app, names)
		},
	}
}

func verifyCassettes(out io.Writer, app *App, names []string) error {
	stale := 0
	for _, n := range names {
		c, err := cassette.Open(filepath.Join(app.Cfg.Cassettes, n))
		if err != nil {
			return err
		}
		es, err := c.Entries()
		if err != nil {
			return err
		}
		// The ruleset version is the cheap half of the check and the half that
		// is always available: an entry recorded under a different version is
		// a claim made under different semantics, whatever its hash says.
		if c.Meta.NormalizeVersion != app.Cfg.Normalize.Version {
			fmt.Fprintf(out, "%s: recorded under ruleset v%d, current is v%d — %d entr%s need re-recording\n",
				n, c.Meta.NormalizeVersion, app.Cfg.Normalize.Version, len(es), plural(len(es)))
			stale += len(es)
			continue
		}
		missing := 0
		for _, e := range es {
			if _, err := os.Stat(c.RequestPath(e.Seq)); err != nil {
				missing++
			}
		}
		if missing > 0 {
			fmt.Fprintf(out, "%s: %d entr%s in the index have no request body on disk\n", n, missing, plural(missing))
			stale += missing
			continue
		}
		fmt.Fprintf(out, "%s: %d entr%s ok\n", n, len(es), plural(len(es)))
	}
	if stale > 0 {
		return &ExitStatus{Code: ExitCassetteMiss,
			Reason: fmt.Sprintf("%d entr%s would no longer match", stale, plural(stale))}
	}
	return nil
}

// scrub is the step between recording a session and committing it.
//
// It reports by default and exits non-zero, so it gates a commit; `--force`
// rewrites. Reporting rather than rewriting is the same shape as `prune`, and
// for a stronger reason: taking a value out of a request changes what replay
// matches on, and whoever runs this has to see what is about to change.
func newCassetteScrubCmd(app *App) *cobra.Command {
	var force bool
	var fromEnv []string
	cmd := &cobra.Command{
		Use:   "scrub [cassette...]",
		Short: "Find credentials and personal data in a cassette, and take them out",
		Long: `Scan every file of a cassette for credentials and personal data, and replace
what it finds with placeholders. Reports and changes nothing unless --force is
given, and exits non-zero while anything is left, so it can gate a commit.

Known credential shapes and email addresses are always looked for. --from-env
names environment variables whose values are also secrets, matched literally —
the value is read from the environment rather than the command line, where every
process on the machine could read it.

A value taken out of a REQUEST changes what replay matches on. That is worth
knowing rather than hiding: such a value was going to make the cassette replay
for nobody but the person who recorded it, and the remedy is a normalize rule,
which blanks it on both sides.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			names := args
			if len(names) == 0 {
				var err error
				if names, err = cassette.List(app.Cfg.Cassettes); err != nil {
					return err
				}
			}
			return scrubCassettes(cmd.OutOrStdout(), app, names, secretsFromEnv(app.getenv, fromEnv), force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "rewrite the files (without this, only report)")
	cmd.Flags().StringSliceVar(&fromEnv, "from-env", nil,
		"environment variables holding secrets to look for, by name (repeatable, or comma-separated)")
	return cmd
}

// secretsFromEnv reads the named variables. A name that is not set is carried
// through with an empty value rather than dropped, so the report can say it was
// asked for and not found.
func secretsFromEnv(getenv func(string) string, names []string) []cassette.Secret {
	out := make([]cassette.Secret, 0, len(names))
	for _, n := range names {
		out = append(out, cassette.Secret{Name: n, Value: getenv(n)})
	}
	return out
}

func scrubCassettes(out io.Writer, app *App, names []string, secrets []cassette.Secret, force bool) error {
	left := 0
	for _, n := range names {
		dir := filepath.Join(app.Cfg.Cassettes, n)
		// Open first: a name that is not a cassette should say so, rather than
		// having its directory walked and reported as clean.
		if _, err := cassette.Open(dir); err != nil {
			return err
		}
		rep, err := cassette.Scrub(dir, secrets, force)
		if err != nil {
			return err
		}
		for _, s := range rep.Skipped {
			fmt.Fprintf(out, "%s: %s was not looked for: %s\n", n, s.Name, s.Why)
		}
		if rep.Total() == 0 {
			fmt.Fprintf(out, "%s: clean\n", n)
			continue
		}
		verb := "found"
		if force {
			verb = "removed"
		}
		for _, f := range rep.Findings {
			fmt.Fprintf(out, "%s: %s %d %s in %s\n", n, verb, f.Count, f.Kind, f.File)
		}
		if force {
			fmt.Fprintf(out, "%s: rewrote %d file%s — re-run the replay before committing\n",
				n, rep.Rewritten, pluralS(rep.Rewritten))
			continue
		}
		left += rep.Total()
	}
	if left > 0 {
		return &ExitStatus{Code: ExitCassetteMiss,
			Reason: fmt.Sprintf("%d value(s) would be published as recorded; re-run with --force to remove them", left)}
	}
	return nil
}

func newCassettePruneCmd(app *App) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "prune <cassette>",
		Short: "Delete body and blob files no index entry references",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pruneCassette(cmd.OutOrStdout(), filepath.Join(app.Cfg.Cassettes, args[0]), force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "actually delete (without this, only report)")
	return cmd
}

func pruneCassette(out io.Writer, dir string, force bool) error {
	c, err := cassette.Open(dir)
	if err != nil {
		return err
	}
	es, err := c.Entries()
	if err != nil {
		return err
	}
	referenced := map[string]bool{}
	for _, e := range es {
		referenced[c.RequestPath(e.Seq)] = true
		referenced[c.ResponsePath(e.Seq, e.Streaming)] = true
	}
	var orphans []string
	for _, sub := range []string{"req", "resp"} {
		ents, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, f := range ents {
			p := filepath.Join(dir, sub, f.Name())
			if !referenced[p] {
				orphans = append(orphans, p)
			}
		}
	}
	// Re-recording appends rather than rewriting, so the previous body files
	// for a re-recorded hash are what this collects.
	if len(orphans) == 0 {
		fmt.Fprintf(out, "%s: nothing to prune\n", dir)
		return nil
	}
	for _, p := range orphans {
		if !force {
			fmt.Fprintf(out, "would remove %s\n", p)
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s\n", p)
	}
	if !force {
		fmt.Fprintf(out, "\n%d file(s); re-run with --force to delete\n", len(orphans))
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// pluralS is the ordinary plural, beside plural()'s "entry"/"entries".
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
