// Package cli implements the charlie command line for the HopCharlie platform.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/cli/internal/update"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitRunFailed   = 3   // a watched run ended other than succeeded
	ExitInterrupted = 130 // Ctrl-C; a run keeps going on the server
)

// requestTimeout bounds every call except event streams and polling loops.
const requestTimeout = 30 * time.Second

// App is one charlie process. Tests replace its I/O, environment and config
// directory.
type App struct {
	Version    string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Getenv     func(string) string
	ConfigDir  func() (string, error) // charlie/config.json lives below it
	HTTPClient *http.Client
	// Updater keeps an installed charlie current; nil turns updates off (tests).
	Updater *update.Config
}

// New returns an App wired to the real process.
func New(version string) *App {
	a := &App{
		Version:    version,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Getenv:     os.Getenv,
		ConfigDir:  os.UserConfigDir,
		HTTPClient: http.DefaultClient,
	}
	if dir, err := os.UserConfigDir(); err == nil {
		u := update.Default(filepath.Join(dir, "charlie"))
		a.Updater = &u
	}
	return a
}

// Run executes args (without the program name) and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	g := &globals{app: a}
	root := g.rootCmd()
	root.SetArgs(args)
	root.SetIn(a.Stdin)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)

	if a.Updater != nil {
		if exe, err := a.Updater.Executable(); err == nil {
			update.CleanOld(exe)
		}
		if msg := update.TakeNotice(*a.Updater); msg != "" {
			fmt.Fprintln(a.Stderr, msg)
		}
	}
	cmd, err := root.ExecuteContextC(ctx)
	code := a.exit(err)
	// After the command, so it never delays the output; the update itself
	// runs in a separate process.
	if a.Updater != nil && (cmd == nil || cmd.Name() != "update") {
		update.MaybeStart(*a.Updater)
	}
	return code
}

func (a *App) exit(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	var ue *usageError
	var se *client.AppSpecError
	switch {
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(a.Stderr, ee.msg)
		}
		return ee.code
	case errors.As(err, &ue):
		fmt.Fprintf(a.Stderr, "error: %v\nRun '%s --help' for usage.\n", ue.err, ue.cmd.CommandPath())
		return ExitUsage
	case errors.As(err, &se):
		// A 422 invalid_app_spec lists every rule platform.yaml breaks.
		fmt.Fprintf(a.Stderr, "error: %v\n%s", err, specErrors(se.Validation.Errors))
		return ExitError
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(a.Stderr, "interrupted")
		return ExitInterrupted
	}
	// API errors read "code: message (HTTP status)".
	fmt.Fprintf(a.Stderr, "error: %v\n", err)
	return ExitError
}

// usageError is a mistake in the command line itself; it exits 2.
type usageError struct {
	cmd *cobra.Command
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func usagef(cmd *cobra.Command, format string, a ...any) error {
	return &usageError{cmd: cmd, err: fmt.Errorf(format, a...)}
}

// exitError ends the process with code after printing msg to stderr.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// globals holds the persistent flags of one invocation.
type globals struct {
	app     *App
	json    bool
	server  string
	noColor bool
}

func (g *globals) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "charlie",
		Short: "Command line for the HopCharlie multi-cloud migration platform",
		Long: `charlie talks to the HopCharlie platform API: environments, apps registered
from GitHub and their deployments, plans, migrations, traffic, the result
matrix and the audit log.

Log in once with "charlie login --server URL"; CHARLIE_SERVER and CHARLIE_TOKEN
override the saved login, and --server overrides both. The saved token is only
sent to the server it was saved for; for another server set CHARLIE_TOKEN or
log in to it.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE:          func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.CompletionOptions.DisableDefaultCmd = true
	pf := root.PersistentFlags()
	pf.BoolVar(&g.json, "json", false, "print the API JSON instead of tables (watch: one JSON object per line)")
	pf.StringVar(&g.server, "server", "", "platform API URL (overrides CHARLIE_SERVER and the saved login)")
	pf.BoolVar(&g.noColor, "no-color", false, "disable colors (NO_COLOR also works)")
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error { return &usageError{cmd: c, err: err} })

	root.AddCommand(
		g.loginCmd(), g.logoutCmd(), g.whoamiCmd(),
		g.envCmd(), g.planCmd(), g.migrateCmd(),
		g.trafficCmd(), g.matrixCmd(), g.auditCmd(), g.versionCmd(), g.updateCmd(),
		g.securityCmd(), g.agentCmd(), g.appCmd(), g.deployCmd(),
	)
	g.addPlanTools(root)
	return root
}

// group returns a command that only holds subcommands.
func group(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(subs...)
	return c
}

// noArgs rejects arguments; for a group it reports an unknown subcommand.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if cmd.HasSubCommands() {
		msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
		if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
			msg += fmt.Sprintf("; did you mean %q?", s[0])
		}
		return &usageError{cmd: cmd, err: errors.New(msg)}
	}
	return usagef(cmd, "%s takes no arguments", cmd.CommandPath())
}

// exactArgs wants one argument per name, e.g. exactArgs("RUNID").
func exactArgs(names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != len(names) {
			return usagef(cmd, "%s needs %s", cmd.CommandPath(), strings.Join(names, " "))
		}
		return nil
	}
}

// settings is the effective server and token.
type settings struct {
	server, token string
	path          string // config file
	savedServer   string // set when the saved token is for another server
}

// settings applies the precedence: --server, then CHARLIE_SERVER and
// CHARLIE_TOKEN, then the saved login. The saved token is used only for the
// saved server.
func (g *globals) settings(cmd *cobra.Command) (settings, error) {
	path, err := configPath(g.app.ConfigDir)
	if err != nil {
		return settings{}, err
	}
	file, err := loadConfig(path)
	if err != nil {
		return settings{}, err
	}
	s := settings{server: file.Server, path: path}
	if v := g.app.Getenv("CHARLIE_SERVER"); v != "" {
		s.server = v
	}
	if g.server != "" {
		u, err := normalizeServer(g.server)
		if err != nil {
			return s, usagef(cmd, "--server: %v", err)
		}
		s.server = u
	} else if s.server != "" {
		u, err := normalizeServer(s.server)
		if err != nil {
			return s, err
		}
		s.server = u
	}
	if file.Token != "" {
		saved, err := normalizeServer(file.Server)
		if err != nil {
			saved = file.Server
		}
		if saved == s.server {
			s.token = file.Token
		} else {
			s.savedServer = saved
		}
	}
	if v := g.app.Getenv("CHARLIE_TOKEN"); v != "" {
		s.token = v
	}
	return s, nil
}

func normalizeServer(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("want an http(s) URL such as http://localhost:8080, got %q", s)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// client returns an API client for the effective settings.
func (g *globals) client(cmd *cobra.Command) (*client.Client, error) {
	s, err := g.settings(cmd)
	if err != nil {
		return nil, err
	}
	if s.server == "" {
		return nil, errors.New(`no server configured; run "charlie login --server URL" or set CHARLIE_SERVER`)
	}
	if s.token == "" && s.savedServer != "" {
		return nil, fmt.Errorf(`the saved login is for %s, not %s; run "charlie login --server %s" or set CHARLIE_TOKEN`,
			s.savedServer, s.server, s.server)
	}
	if s.token == "" {
		return nil, fmt.Errorf(`not logged in to %s; run "charlie login" or set CHARLIE_TOKEN`, s.server)
	}
	return g.newClient(s.server, s.token), nil
}

func (g *globals) newClient(server, token string) *client.Client {
	return client.New(server, token,
		client.WithHTTPClient(g.app.HTTPClient),
		client.WithUserAgent("charlie/"+g.app.Version))
}

func callCtx(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), requestTimeout)
}

func (g *globals) out() *printer {
	color := !g.noColor && g.app.Getenv("NO_COLOR") == "" && ansiTerminal(g.app.Stdout)
	return &printer{w: g.app.Stdout, color: color}
}

// show prints v as JSON with --json, otherwise render(v).
func show[T any](g *globals, v *T, render func(*T) string) error {
	p := g.out()
	if g.json {
		return p.json(v)
	}
	return p.text(render(v))
}
