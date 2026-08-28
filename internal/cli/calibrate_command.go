package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/spf13/cobra"
)

// `cs-vcr calibrate` turns a failed replay into the configuration that would
// have let it pass, for review.
//
// It exists because the alternative is knowing each agent's internal protocol.
// Finding that Codex feeds its own tool-call timings back into the next request
// took three rounds of recording, replaying and diffing by hand; this is that
// procedure, done mechanically, from files the replay already wrote.
//
// It proposes and never applies. The output is a config diff to read in a PR
// next to the cassette it came from, and the judgement it cannot make is the
// one that matters: whether a path that differed is the world answering
// differently, or the agent being asked something else.
func newCalibrateCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "calibrate <cassette> <miss-dir>",
		Short: "Propose normalize.volatile paths from a failed replay",
		Long: `Compare the requests a replay dumped against the steps they were compared
against, and propose the paths that differed as normalize.volatile rules.

    cs-vcr replay --dump-misses ./misses    # fails, dumps
    cs-vcr calibrate build ./misses         # proposes rules

Contacts no provider and changes no file. What it prints is a proposal to read
and decide on: a path is only volatile if what differs there is something the
world decides rather than something the agent asked.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cassette.Open(filepath.Join(app.Cfg.Cassettes, args[0]))
			if err != nil {
				return err
			}
			return calibrate(cmd.OutOrStdout(), c, args[1], app.Cfg.Normalize.VolatilePaths())
		},
	}
}

// proposal is one path worth declaring, and the evidence for it.
type proposal struct {
	path  string
	count int
	// example is one pair of values, which is what tells a reader whether the
	// path is the world's answer or the agent's ask. A count alone does not.
	recorded, live string
}

func calibrate(out io.Writer, c *cassette.Cassette, missDir string, declared []string) error {
	entries, err := c.Entries()
	if err != nil {
		return err
	}
	bySeq := map[int]cassette.Entry{}
	for _, e := range entries {
		bySeq[e.Seq] = e
	}

	files, err := os.ReadDir(missDir)
	if err != nil {
		return err
	}

	byPath := map[string]*proposal{}
	var shapes []string
	var unpaired, paired int

	for _, f := range files {
		name := strings.TrimSuffix(f.Name(), ".json")
		seq, err := strconv.Atoi(name)
		if err != nil {
			// A request that matched no step: there is nothing to compare it
			// against, and pairing it with a turn by guesswork is how a
			// proposal names the wrong path with total confidence.
			unpaired++
			continue
		}
		e, ok := bySeq[seq]
		if !ok {
			unpaired++
			continue
		}
		recorded, err := os.ReadFile(c.RequestPath(e.Seq))
		if err != nil {
			return err
		}
		live, err := os.ReadFile(filepath.Join(missDir, f.Name()))
		if err != nil {
			return err
		}
		// Aligned against what is ALREADY declared, so the proposal is what is
		// still missing rather than a re-listing of the ruleset in force.
		al, err := cassette.Align(recorded, live, cassette.Rules(declared))
		if err != nil {
			return fmt.Errorf("step %d: %w", e.Seq, err)
		}
		paired++
		for _, d := range al.Leaf {
			p := cassette.Generalize(d.Path)
			if byPath[p] == nil {
				byPath[p] = &proposal{path: p,
					recorded: cassette.Short(d.Recorded), live: cassette.Short(d.Live)}
			}
			byPath[p].count++
		}
		for _, d := range al.Shape {
			shapes = append(shapes, fmt.Sprintf("  step %d  %s: %s", e.Seq, d.Path, d.Why))
		}
	}

	return report(out, byPath, shapes, paired, unpaired)
}

func report(out io.Writer, byPath map[string]*proposal, shapes []string, paired, unpaired int) error {
	props := make([]*proposal, 0, len(byPath))
	for _, p := range byPath {
		props = append(props, p)
	}
	sort.Slice(props, func(i, j int) bool { return props[i].path < props[j].path })

	fmt.Fprintf(out, "%d request(s) paired with a recorded step", paired)
	if unpaired > 0 {
		// Said out loud: these carry no proposal, and a reader who assumed the
		// output covered every miss would go looking for a rule that is not
		// there.
		fmt.Fprintf(out, ", %d matched no step and were skipped", unpaired)
	}
	fmt.Fprintln(out)

	// Shape differences first, and never as a proposal. An item added or gone
	// means the request is BUILT differently, and there is no rule for that —
	// declaring the list it happened in would blank the list.
	if len(shapes) > 0 {
		fmt.Fprintf(out, "\n%d difference(s) no rule can cover — the request is built differently:\n", len(shapes))
		for _, s := range shapes {
			fmt.Fprintln(out, s)
		}
	}

	if len(props) == 0 {
		if len(shapes) == 0 {
			fmt.Fprintln(out, "\nnothing to propose: every paired request already aligns.")
		}
		return nil
	}

	// Printed as config that parses, because pasting it is the whole point:
	// `volatile` is a list of paths, and the evidence goes above each as a
	// comment rather than beside it as a field that does not exist.
	//
	// Under `extend`, because a bare `volatile` REPLACES the shipped list. This
	// output is meant to be pasted, and pasting it used to leave a deployment
	// with these paths and none of the four cs-vcr ships. Those four are the
	// tool results, so the paste fixed one miss and caused a miss on every
	// multi-turn request after it.
	fmt.Fprintf(out, "\n%d path(s) differed. Declare the ones the WORLD decides:\n\n", len(props))
	fmt.Fprintln(out, "normalize:")
	fmt.Fprintln(out, "  extend:")
	fmt.Fprintln(out, "    volatile:")
	for _, p := range props {
		fmt.Fprintf(out, "      # %d difference(s), e.g.\n", p.count)
		fmt.Fprintf(out, "      #   recorded: %s\n", p.recorded)
		fmt.Fprintf(out, "      #   this run: %s\n", p.live)
		fmt.Fprintf(out, "      - %s\n", p.path)
	}
	return nil
}
