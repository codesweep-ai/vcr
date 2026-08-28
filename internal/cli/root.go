// Package cli wires the cobra command tree over the internal packages
// (cli depends on config/proxy; main only maps exit codes).
package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/codesweep-ai/vcr"
	"github.com/codesweep-ai/vcr/internal/config"
	"github.com/codesweep-ai/vcr/internal/paths"
	"github.com/spf13/cobra"
)

// devVersion marks a binary that carried no release stamp.
const devVersion = "dev"

// Version is the tool version (set via -ldflags at release).
var Version = devVersion

// buildVersion reports the release stamp when there is one, and otherwise the
// module version the toolchain recorded. A binary installed straight from the
// module path carries no stamp, so without this it would answer the dev
// sentinel and leave the provenance of a recording unsayable.
func buildVersion() string {
	if Version != devVersion {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return Version
	}
	return info.Main.Version
}

// App holds process-wide dependencies resolved once at startup.
type App struct {
	Cfg  *config.Config
	Log  *slog.Logger
	Path string // the config file that was loaded (or would have been)

	Getenv func(string) string // injected in tests
}

func (a *App) getenv(k string) string {
	if a.Getenv != nil {
		return a.Getenv(k)
	}
	return os.Getenv(k)
}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command { return newRootCmd(&App{}) }

// newRootCmd builds the tree over the given App. Tests pass a pre-wired App (a
// fake environment, a buffer errW) to drive commands without touching the real
// config file or the real network.
func newRootCmd(app *App) *cobra.Command {
	var (
		cfgPath string
		verbose bool
		quiet   bool
		logJSON bool
	)

	root := &cobra.Command{
		Use:   "cs-vcr",
		Short: "Record and replay the traffic between an AI coding agent and its LLM provider",
		Long: `cs-vcr sits between an AI coding agent and an LLM provider. It records what
passes through into a cassette, and replays it later, so a run can exercise the
whole agent loop without calling a provider.

The agent keeps whatever login it already has: cs-vcr forwards credentials
untouched and never records a request header.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if quiet && verbose {
				return errors.New("--quiet and --verbose are mutually exclusive")
			}
			app.Log = newLogger(os.Stderr, verbose, quiet, logJSON)

			// A file someone named has to be there, whether they named it with
			// the flag or with the variable. The default location usually holds
			// none, and that is not an error — cs-vcr has to run with no
			// configuration at all — but naming one is a caller saying where
			// their settings live, and a session that shrugged at a typo would
			// run on settings they did not write.
			app.Path = cfgPath
			namedBy := "--config"
			if app.Path == "" {
				var named bool
				if app.Path, named = paths.Config(); !named {
					namedBy = ""
				} else {
					namedBy = "CS_VCR_CONFIG"
				}
			}
			if namedBy != "" {
				if _, err := os.Stat(app.Path); errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("%s %s: no such file; without it cs-vcr runs on the defaults",
						namedBy, app.Path)
				}
			}
			cfg, err := config.Load(app.Path)
			if err != nil {
				return err
			}
			if err := cfg.ApplyEnv(app.getenv); err != nil {
				return err
			}
			app.Cfg = cfg
			return nil
		},
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "", "config file (default: $XDG_CONFIG_HOME/cs-vcr/config.yaml)")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output (debug logging)")
	root.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "silence everything below error")
	root.PersistentFlags().BoolVar(&logJSON, "log-json", false, "structured JSON logs instead of text")

	root.AddCommand(newVersionCmd())
	root.AddCommand(newManualCmd())
	root.AddCommand(newRecordCmd(app))
	root.AddCommand(newReplayCmd(app))
	root.AddCommand(newConfigCmd(app))
	root.AddCommand(newCassetteCmd(app))
	root.AddCommand(newCalibrateCmd(app))
	return root
}

// newLogger builds the session logger. Its output is structured, and never
// carries request or response bodies at default verbosity — which is a property of
// what the call sites pass, so the handler's job is only the level.
func newLogger(w io.Writer, verbose, quiet, asJSON bool) *slog.Logger {
	level := slog.LevelInfo
	switch {
	case quiet:
		level = slog.LevelError
	case verbose:
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if asJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cs-vcr version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "cs-vcr %s (%s/%s, %s)\n",
				buildVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
			return nil
		},
	}
}

// newManualCmd prints the embedded MANUAL.md. The binary is the whole tool: a
// user who downloaded it, and an agent working in a checkout that does not have
// the docs, both read the reference by running it.
func newManualCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manual",
		Short: "Print the cs-vcr manual",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), vcr.ManualMD)
			return err
		},
	}
}

// newConfigCmd prints configuration: cs-vcr's own with no argument, and an
// agent's with one.
//
// They are the same question asked from the two ends of the connection. Without
// an argument it answers "which of the file, the environment and the flags
// won", which is where a base URL that did not behave as expected leads. With
// an agent it answers "what do I put in front of this client to reach that
// cassette", which is where every first session starts and where the one rule
// nothing else states in one place lives: whether the client wants a /v1 after
// the cassette name.
func newConfigCmd(app *App) *cobra.Command {
	var cassette, provider, url string
	var envOnly bool
	cmd := &cobra.Command{
		Use:   "config [AGENT]",
		Short: "Print the resolved configuration, or how to point an agent at a cassette",
		Long: `With no argument, print the configuration cs-vcr resolved from its file, the
environment and the flags.

With an agent — ` + strings.Join(agentNames(), ", ") + ` — print how to point that client at this
cs-vcr and at one cassette: a command to run, the environment to set instead,
and the file to pin it in. The provider and the cassette are named by a
` + config.Prefix + `<provider>/<cassette> prefix on the base URL, and where that goes relative
to the /v1 a client appends differs by client, which is what this prints.

Environment lines are bare VAR=VALUE, so they can be read by a shell, a dotenv
file or a CI environment block:

  set -a; . <(cs-vcr config claude --cassette build --provider anthropic --env-only); set +a`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.Cfg.Resolve(); err != nil {
				return err
			}
			if len(args) == 0 {
				return printConfig(cmd.OutOrStdout(), app)
			}
			a, ok := findAgent(args[0])
			if !ok {
				return fmt.Errorf("no settings for an agent called %q; cs-vcr knows %s",
					args[0], strings.Join(agentNames(), ", "))
			}
			if cassette == "" {
				return errors.New("which cassette? pass --cassette — the printed base URL names it")
			}
			if err := config.CheckCassetteName(cassette); err != nil {
				return err
			}
			if provider == "" {
				return errors.New("which provider? pass --provider — the printed base URL names it too")
			}
			if err := config.CheckProviderName(provider); err != nil {
				return err
			}
			at := url
			if at == "" {
				at = proxyURL(app.Cfg.Listen)
			}
			return printAgentConfig(cmd.OutOrStdout(), a, at, cassette, provider, envOnly)
		},
	}
	cmd.Flags().StringVar(&cassette, "cassette", "", "cassette the printed base URL names (required)")
	cmd.Flags().StringVar(&provider, "provider", "", "provider entry the printed base URL names (required)")
	cmd.Flags().StringVar(&url, "url", "", "where the agent reaches cs-vcr (default: derived from listen)")
	cmd.Flags().BoolVar(&envOnly, "env-only", false, "print only the VAR=VALUE lines, for sourcing or piping")
	return cmd
}
