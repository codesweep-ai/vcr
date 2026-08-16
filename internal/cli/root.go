// Package cli wires the cobra command tree over the internal packages
// (cli depends on config/proxy; main only maps exit codes).
package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"

	"github.com/codesweep-ai/vcr"
	"github.com/codesweep-ai/vcr/internal/config"
	"github.com/codesweep-ai/vcr/internal/paths"
	"github.com/spf13/cobra"
)

// Version is the tool version (set via -ldflags at release).
var Version = "0.1.0-dev"

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

			app.Path = cfgPath
			if app.Path == "" {
				app.Path = paths.Config()
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
				Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
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

// newConfigCmd prints the resolved configuration. It is the answer to "which
// of the file, the environment and the flags won" — the question a base URL
// that did not behave as expected leads to.
func newConfigCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print the resolved configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.Cfg.Resolve(); err != nil {
				return err
			}
			return printConfig(cmd.OutOrStdout(), app)
		},
	}
	return cmd
}
