package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/codesweep-ai/vcr/internal/cassette"
	"github.com/codesweep-ai/vcr/internal/config"
	"github.com/codesweep-ai/vcr/internal/proxy"
	"github.com/spf13/cobra"
)

// Two commands, not one command with a switch. Which one you typed is the whole
// difference, and it is the difference that decides whether money can be spent:
//
//	cs-vcr record   calls providers, and writes what comes back
//	cs-vcr replay    serves only from the cassette, and can reach nothing
//
// The guarantee is then structural. There is no flag to get wrong, no config
// file that can quietly say otherwise, and no environment variable to forget — a
// replay server is built without the ability to dial out, and the safety of a
// pipeline is visible in the command line it runs.
func newRecordCmd(app *App) *cobra.Command { return newServeCmd(app, false) }
func newReplayCmd(app *App) *cobra.Command { return newServeCmd(app, true) }

func newServeCmd(app *App, offline bool) *cobra.Command {
	use, short, long := "record", "Record LLM traffic into a cassette",
		`Proxy an agent's LLM traffic to the provider, appending every interaction to
a cassette. It consults the cassette for nothing: each call the session makes,
including one it makes twice, reaches the provider, because a cassette is the
record of a session rather than a cache.

Point an agent at it with the base-URL variable for its provider, carrying
/c/<provider>/<cassette> to say where the traffic goes and which cassette the
run belongs to:

  ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/anthropic/build

The provider names an entry under "providers". Nothing declares the cassette:
this command creates it on the first request that asks for it, and a second
cassette is a second base URL, with no restart in between. Where the prefix goes
relative to the /v1 a client appends differs by client, so run
"cs-vcr config <agent>" for the exact URL.

A request whose base URL carries no prefix is refused: cs-vcr does not guess
which cassette it meant. Nothing else about the agent changes, and it keeps
whatever login it has.`
	if offline {
		use, short, long = "replay", "Replay a cassette, contacting no provider",
			`Serve an agent's LLM traffic entirely from a cassette. No provider is ever
contacted: a request with no recording fails the run with a diff against the
nearest entry, and the session exits non-zero.

This is the command a pipeline runs. It cannot spend money — not because it is
configured not to, but because it is not built with anywhere to send a request.

The base URL says which cassette to serve, by carrying
/c/<provider>/<cassette>. This command reads no provider configuration, so the
provider segment only has to be there:

  ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/anthropic/build

A cassette the store does not hold is refused and named, rather than created and
then missed on every request, and so is a request whose base URL carries no
prefix at all. Run "cs-vcr config <agent>" for the exact URL a client wants.`
	}
	var listen, admin, cassettes, dumpMisses string
	cmd := &cobra.Command{
		Use: use, Short: short, Long: long,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := app.Cfg
			if listen != "" {
				cfg.Listen = listen
			}
			if admin != "" {
				cfg.Admin = admin
			}
			if cassettes != "" {
				cfg.Cassettes = cassettes
			}
			// Which checks the configuration gets is the same decision as
			// which command this is. `record` forwards, so every provider it
			// could route to has to be usable; `replay` reads none of them, and
			// has to start with none configured.
			resolve := cfg.Resolve
			if offline {
				resolve = cfg.ResolveOffline
			}
			if err := resolve(); err != nil {
				return err
			}
			return runServe(cmd.Context(), app, cmd.OutOrStdout(), offline, dumpMisses)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address for the proxied port (default 127.0.0.1:8080)")
	cmd.Flags().StringVar(&admin, "admin", "", "address for /healthz (default 127.0.0.1:8081)")
	cmd.Flags().StringVar(&cassettes, "cassettes", "", "directory holding cassettes (default ./cassettes)")
	if offline {
		// Only on replay: it is the command that can miss.
		cmd.Flags().StringVar(&dumpMisses, "dump-misses", "",
			"write each missed request's normalized body to this directory, to diff against the cassette")
	}
	return cmd
}

func runServe(ctx context.Context, app *App, out io.Writer, offline bool, dumpMisses string) error {
	cfg := app.Cfg
	srv := proxy.New(cfg, app.Log, offline)

	// How a cassette is opened is the command's decision, and it is the same
	// decision as whether money can be spent: `record` creates what is not
	// there, `replay` refuses it. Replay creating one would turn a base URL
	// with a typo in it into a session that missed every request, which reads
	// as an agent that diverged.
	open := func(name string) (*cassette.Store, error) {
		dir := filepath.Join(cfg.Cassettes, name)
		if offline {
			store, err := cassette.OpenExistingStore(dir, cfg.Normalize.Version)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s", proxy.ErrNoSuchCassette, dir)
			}
			return store, err
		}
		return cassette.OpenStore(dir, buildVersion(), cfg.Normalize.Version,
			func() int64 { return time.Now().Unix() })
	}
	srv = srv.WithOpener(open)

	if dumpMisses != "" {
		srv = srv.WithMissDump(dumpMisses)
	}

	// /healthz on its own listener, so a CI script can wait for the proxy to be
	// ready before it starts the build. It cannot live on the proxied port:
	// anything unrecognized there is forwarded to a provider, so /healthz would
	// become a request to Anthropic.
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Both listeners are opened before either server runs, so that a port
	// clash fails startup instead of leaving the proxy up and the admin
	// endpoint silently missing.
	pl, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	defer pl.Close()
	al, err := net.Listen("tcp", cfg.Admin)
	if err != nil {
		return fmt.Errorf("listen on %s (admin): %w", cfg.Admin, err)
	}
	defer al.Close()

	proxySrv := &http.Server{Handler: srv, ErrorLog: slog.NewLogLogger(app.Log.Handler(), slog.LevelError)}
	adminSrv := &http.Server{Handler: adminMux, ErrorLog: slog.NewLogLogger(app.Log.Handler(), slog.LevelError)}

	attrs := []any{
		slog.Bool("offline", offline),
		slog.String("listen", pl.Addr().String()),
		slog.String("admin", al.Addr().String()),
	}
	if !offline {
		// Only a session that forwards has providers. Counting them under
		// `replay` would describe configuration this session never read.
		attrs = append(attrs, slog.Int("providers", len(cfg.Providers)))
	}
	app.Log.Info("serving", attrs...)
	// A config file that stood in for a shipped list rather than extending it.
	// Said out loud because the alternative is silent: the rules it dropped are
	// the ones that make a multi-turn session replay, and their absence shows up
	// as a miss on a request nobody edited.
	if replaced := cfg.Normalize.ReplacedShipped(); len(replaced) > 0 {
		app.Log.Warn("this config replaces rules cs-vcr ships rather than adding to them",
			slog.String("fields", strings.Join(replaced, ", ")),
			slog.String("add_instead", "normalize.extend"))
	}
	if offline {
		// The property that makes a pipeline harmless, stated out loud so a log
		// reader can confirm it held.
		app.Log.Info("replay: no provider will be contacted this session")
	}

	errc := make(chan error, 2)
	go func() { errc <- serveIgnoringClose(proxySrv, pl) }()
	go func() { errc <- serveIgnoringClose(adminSrv, al) }()

	select {
	case <-ctx.Done():
		// From here a second signal means "now, whatever is in flight". While
		// NotifyContext is armed it would be swallowed instead, and the drain
		// below would read as a hang with no way out of it.
		stop()
	case err := <-errc:
		if err != nil {
			return err
		}
	}

	drain(proxySrv, drainTimeout)
	// The admin listener carries /healthz, which has nothing worth waiting for.
	drain(adminSrv, time.Second)

	// Printed on the way out rather than logged, because it is the
	// artifact a human reads after the run, not an event during it.
	return summarize(out, srv.Snapshot(), offline)
}

// drainTimeout bounds how long a stopping session waits for the requests still
// being answered.
//
// It is long because of what is being waited for. A request in flight is a
// recording not yet written: the entry is appended once the response is done,
// so exiting underneath one loses the whole interaction — the provider was
// called, the tokens were spent, and the cassette keeps no trace of it. The
// five seconds this used to allow are generous for an idle server and nowhere
// near a model composing an answer, so a Ctrl-C during a turn reliably threw
// that turn away.
//
// What bounds an operator's patience is the second Ctrl-C, not this.
//
// A var only so a test can shorten it; nothing in the program writes it.
var drainTimeout = 2 * time.Minute

func drain(s *http.Server, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func serveIgnoringClose(s *http.Server, l net.Listener) error {
	if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// summarize prints the session summary and decides the exit status.
func summarize(out io.Writer, st proxy.Stats, offline bool) error {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	verb := "record"
	if offline {
		verb = "replay"
	}
	// No cassette is named in the heading: a session serves whichever ones its
	// requests asked for, and the per-cassette lines at the bottom say which.
	fmt.Fprintf(tw, "\ncs-vcr %s summary\n", verb)
	fmt.Fprintf(tw, "requests\t%d\n", st.Requests)
	fmt.Fprintf(tw, "replayed\t%d\n", st.Replayed)
	fmt.Fprintf(tw, "recorded\t%d\n", st.Recorded)
	fmt.Fprintf(tw, "upstream calls\t%d\n", st.Upstream)
	fmt.Fprintf(tw, "misses\t%d\n", st.Misses)
	fmt.Fprintf(tw, "unknown cassette\t%d\n", st.UnknownCassette)
	fmt.Fprintf(tw, "rejected\t%d\n", st.Rejected)
	// Only when it happened. A zero is the ordinary case and would be one more
	// number to read past; a non-zero is a provider call this session paid for
	// and did not keep, which is otherwise visible only as arithmetic that does
	// not add up.
	if st.InFlight > 0 {
		fmt.Fprintf(tw, "abandoned\t%d\n", st.InFlight)
	}
	// Both only when they happened. A drifted observation is the world having
	// moved under the cassette, and an out-of-order step is a client that
	// pipelined — neither fails a run, and both are things a reader wants to
	// know before trusting one that passed.
	if st.Drifted > 0 {
		fmt.Fprintf(tw, "drifted observations\t%d\n", st.Drifted)
	}
	// The one place replay serves a response the request did not align with,
	// so it is reported wherever a reader judges a run: a count that only
	// reached the log would make the tolerance effectively silent, which is the
	// thing that made defaulting it on defensible in the first place.
	if st.Auxiliary > 0 {
		fmt.Fprintf(tw, "bookkeeping calls answered\t%d\n", st.Auxiliary)
	}
	// Only when the session was used as a proxy, which most are not. A tunnel is
	// the one path through cs-vcr that records nothing, so these are the only
	// place that traffic is visible after the fact.
	if st.TunnelOpened > 0 || st.TunnelBlocked > 0 {
		fmt.Fprintf(tw, "tunnelled\t%d\n", st.TunnelOpened)
		fmt.Fprintf(tw, "tunnel refused\t%d\n", st.TunnelBlocked)
	}
	// Its own line, and worded as a warning rather than a count, because it is
	// the one tolerance that can cost a session its result: the client was
	// handed the answer to a command that did not succeed here. A run can pass
	// with these and often does -- a recorded command naming the recording's
	// own commit will always fail on a fresh checkout -- but a reader deciding
	// whether to trust a green replay needs to know it happened.
	if st.ToleratedFailures > 0 {
		fmt.Fprintf(tw, "commands that failed here\t%d\t(succeeded when recorded; answers were replayed anyway)\n", st.ToleratedFailures)
	}
	if st.OutOfOrder > 0 {
		fmt.Fprintf(tw, "out of recorded order\t%d\n", st.OutOfOrder)
	}
	for _, surface := range slices.Sorted(maps.Keys(st.BySurface)) {
		fmt.Fprintf(tw, "  surface %s\t%d\n", surface, st.BySurface[surface])
	}
	// Sorted, because this is now as long as the session had cassettes rather
	// than as long as the config, and map order would reshuffle it every run.
	for _, name := range slices.Sorted(maps.Keys(st.ByCassette)) {
		fmt.Fprintf(tw, "  cassette %s\t%d\n", name, st.ByCassette[name])
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// A replay session that could not serve a request has to fail the build,
	// or a cassette that stopped matching becomes a silently green pipeline.
	if offline && st.Misses > 0 {
		return &ExitStatus{Code: ExitCassetteMiss,
			Reason: fmt.Sprintf("%d request(s) had no recording", st.Misses)}
	}
	return nil
}

// printConfig renders the resolved configuration for `cs-vcr config`.
func printConfig(out io.Writer, app *App) error {
	cfg := app.Cfg
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "config file\t%s\n", app.Path)
	fmt.Fprintf(tw, "listen\t%s\n", cfg.Listen)
	fmt.Fprintf(tw, "admin\t%s\n", cfg.Admin)
	fmt.Fprintf(tw, "cassettes\t%s\n", cfg.Cassettes)
	fmt.Fprintf(tw, "normalize ruleset\tv%d (%d strip, %d query, %d replace)\n",
		cfg.Normalize.Version, len(cfg.Normalize.Strip), len(cfg.Normalize.Query), len(cfg.Normalize.Replace))
	if replaced := cfg.Normalize.ReplacedShipped(); len(replaced) > 0 {
		fmt.Fprintf(tw, "\treplaces the shipped %s (use normalize.extend to add instead)\n",
			strings.Join(replaced, ", "))
	}
	fmt.Fprintf(tw, "normalize root\t%s\n", orDash(cfg.Normalize.Root))
	fmt.Fprintf(tw, "base URL prefix\t%s<provider>/<cassette>\n", config.Prefix)
	fmt.Fprintln(tw, "\nPROVIDER\tBASE URL")
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		fmt.Fprintf(tw, "%s\t%s\n", name, cfg.Providers[name].BaseURL)
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
