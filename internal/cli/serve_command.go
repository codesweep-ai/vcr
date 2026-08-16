package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"path/filepath"
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
		`Proxy an agent's LLM traffic, serving what the cassette already holds and
calling the provider for what it does not. Point an agent at it with the
base-URL variable for its provider; nothing else changes, and the agent keeps
whatever login it has:

  ANTHROPIC_BASE_URL=http://127.0.0.1:8080
  OPENAI_BASE_URL=http://127.0.0.1:8080

Add /c/<name> to the base URL to record several agents into separate cassettes.`
	if offline {
		use, short, long = "replay", "Replay a cassette, contacting no provider",
			`Serve an agent's LLM traffic entirely from a cassette. No provider is ever
contacted: a request with no recording fails the run with a diff against the
nearest entry, and the session exits non-zero.

This is the command a pipeline runs. It cannot spend money — not because it is
configured not to, but because it is not built with anywhere to send a request.`
	}
	var listen, admin, cassette, cassettes, dumpMisses string
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
			if cassette != "" {
				cfg.Cassette = cassette
			}
			if cassettes != "" {
				cfg.Cassettes = cassettes
			}
			if err := cfg.Resolve(); err != nil {
				return err
			}
			return runServe(cmd.Context(), app, cmd.OutOrStdout(), offline, dumpMisses)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address for the proxied port (default 127.0.0.1:8080)")
	cmd.Flags().StringVar(&admin, "admin", "", "address for /healthz (default 127.0.0.1:8081)")
	cmd.Flags().StringVar(&cassette, "cassette", "", "cassette this session uses (default \"default\")")
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

	// The cassette, unless this session neither records nor replays. Opening it
	// at startup rather than on the first request means a bad path or an
	// unreadable cassette fails the run immediately, instead of turning into a
	// storm of misses halfway through a CI job.
	// A cassette only where one was named. Naming one is what asks for
	// recording; without a name there is nothing to record into and cs-vcr is
	// simply a proxy.
	open := func(name string) (*cassette.Store, error) {
		return cassette.OpenStore(filepath.Join(cfg.Cassettes, name),
			Version, cfg.Normalize.Version, func() int64 { return time.Now().Unix() })
	}
	if cfg.Cassette != "" {
		store, err := open(cfg.Cassette)
		if err != nil {
			return fmt.Errorf("cassette %s: %w", cfg.Cassette, err)
		}
		srv = srv.WithCassette(store)
		app.Log.Info("cassette open",
			slog.String("name", cfg.Cassette), slog.Int("entries", store.Len()))
	}
	// A cassette per client where one is named. Several agents through one
	// cs-vcr is the campaign case, and they must not share a key namespace:
	// an orchestrator and the member it delegates to receive the same opening
	// prompt, so their first requests normalize to identical bytes.
	for _, cl := range cfg.Clients {
		if cl.Cassette == "" {
			continue
		}
		store, err := open(cl.Cassette)
		if err != nil {
			return fmt.Errorf("cassette %s (client %s): %w", cl.Cassette, cl.Label, err)
		}
		srv = srv.WithClientCassette(cl.Label, store)
		app.Log.Info("cassette open",
			slog.String("name", cl.Cassette), slog.String("client", cl.Label),
			slog.Int("entries", store.Len()))
	}
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

	app.Log.Info("serving",
		slog.Bool("offline", offline),
		slog.String("cassette", cfg.Cassette),
		slog.Int("clients", len(cfg.Clients)),
		slog.String("listen", pl.Addr().String()),
		slog.String("admin", al.Addr().String()),
		slog.Int("providers", len(cfg.Providers)))
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
	return summarize(out, srv.Snapshot(), cfg, offline)
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
func summarize(out io.Writer, st proxy.Stats, cfg *config.Config, offline bool) error {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	verb := "record"
	if offline {
		verb = "replay"
	}
	fmt.Fprintf(tw, "\ncs-vcr %s summary (cassette=%s)\n", verb, orDash(cfg.Cassette))
	fmt.Fprintf(tw, "requests\t%d\n", st.Requests)
	fmt.Fprintf(tw, "replayed\t%d\n", st.Replayed)
	fmt.Fprintf(tw, "recorded\t%d\n", st.Recorded)
	fmt.Fprintf(tw, "upstream calls\t%d\n", st.Upstream)
	fmt.Fprintf(tw, "misses\t%d\n", st.Misses)
	fmt.Fprintf(tw, "unmatched client\t%d\n", st.Unmatched)
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
	if st.OutOfOrder > 0 {
		fmt.Fprintf(tw, "out of recorded order\t%d\n", st.OutOfOrder)
	}
	for surface, n := range st.BySurface {
		fmt.Fprintf(tw, "  surface %s\t%d\n", surface, n)
	}
	for label, n := range st.ByLabel {
		fmt.Fprintf(tw, "  client %s\t%d\n", label, n)
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
	fmt.Fprintf(tw, "cassette\t%s\n", orDash(cfg.Cassette))
	fmt.Fprintf(tw, "default provider\t%s\n", orDash(cfg.DefaultProvider))
	fmt.Fprintf(tw, "normalize ruleset\tv%d (%d strip, %d query, %d replace)\n",
		cfg.Normalize.Version, len(cfg.Normalize.Strip), len(cfg.Normalize.Query), len(cfg.Normalize.Replace))
	fmt.Fprintf(tw, "normalize root\t%s\n", orDash(cfg.Normalize.Root))
	fmt.Fprintln(tw, "\nPROVIDER\tBASE URL")
	for name, p := range cfg.Providers {
		fmt.Fprintf(tw, "%s\t%s\n", name, p.BaseURL)
	}
	if len(cfg.Clients) > 0 {
		fmt.Fprintln(tw, "\nCLIENT\tMATCHES\tCASSETTE\tPROVIDER")
		for _, cl := range cfg.Clients {
			match := cl.Match.PathPrefix
			if match == "" {
				match = "(everything)"
			}
			provider := cl.Provider
			if provider == "" {
				provider = "(by path)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cl.Label, match, orDash(cl.Cassette), provider)
		}
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
