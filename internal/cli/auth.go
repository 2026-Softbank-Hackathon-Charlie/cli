package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

// defaultServer is the API of the team's deployment, which "charlie login"
// uses when no server is configured.
const defaultServer = "https://api.hopcharlie.dev"

const (
	// approvalGrace is how long after the code's expiry an approved device
	// login can still be exchanged: approving extends the window by this.
	approvalGrace = 10 * time.Minute
	// maxClientChars is the server's limit for the client description.
	maxClientChars = 200
	// logoutTimeout bounds the best-effort logout call.
	logoutTimeout = 5 * time.Second
)

var errDenied = errors.New("an administrator denied this login in the console")

func (g *globals) loginCmd() *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{
		Use:   "login [--server URL] [--no-wait]",
		Short: "Log in with a code that an administrator approves in the console",
		Long: `Logs in with a device code (after RFC 8628). charlie asks the server for a
code and shows it; an administrator logged in to the web console enters the
code, picks the role this CLI gets (operator, agent or viewer) and approves.
charlie then saves the token the server hands out (valid for 30 days) to
config.json (mode 0600) in the charlie config directory, which
CHARLIE_CONFIG_DIR sets.

The server is --server, else CHARLIE_SERVER, else the saved login, else
` + defaultServer + `.

Until it is approved, denied or expired, the code is kept in
pending-login.json, so "charlie login" in another process shows the same code
and finishes the login. Bots and remote hosts that cannot wait log in in two
steps:

  charlie login --no-wait    # prints the code and exits
  charlie login              # after the approval, saves the token

Programs with a static token set CHARLIE_TOKEN instead of logging in.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.settings(cmd)
			if err != nil {
				return err
			}
			server := cmp.Or(s.server, defaultServer)
			l := &deviceLogin{
				g:           g,
				server:      server,
				c:           g.newClient(server, ""),
				configPath:  s.path,
				pendingPath: filepath.Join(filepath.Dir(s.path), pendingFile),
			}
			return l.run(cmd.Context(), noWait)
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, `print the code and exit; after the approval, "charlie login" finishes`)
	return cmd
}

// deviceLogin is one "charlie login".
type deviceLogin struct {
	g           *globals
	server      string
	c           *client.Client
	configPath  string
	pendingPath string
	noted       bool // a retry note was printed
}

// loginResult is what login prints with --json, and whoami.
type loginResult struct {
	apiv1.Principal
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// pendingCode is what "login --no-wait --json" prints. It never contains the
// device code.
type pendingCode struct {
	UserCode        string    `json:"userCode"`
	VerificationURI string    `json:"verificationUri"`
	ExpiresAt       time.Time `json:"expiresAt"`
	Server          string    `json:"server"`
}

func (l *deviceLogin) run(ctx context.Context, noWait bool) error {
	p, err := loadPending(l.pendingPath)
	if err != nil {
		return fmt.Errorf("read the pending login: %w", err)
	}
	var retry error // the resume poll could not reach the server
	if p != nil && p.Server == l.server {
		// The server decides whether the pending login is over, not the
		// local clock: an approval extends the exchange window.
		s, err := l.poll(ctx, p)
		switch code := client.ErrorCode(err); {
		case err == nil:
			return l.finish(s)
		case code == "authorization_pending":
		case code == "expired_token":
			l.forget()
			p = nil // start a new one below
		case code == "access_denied":
			l.forget()
			return errDenied
		case ctx.Err() != nil:
			return l.interrupted()
		case noWait || !transient(err):
			return fmt.Errorf("check the pending login with %s: %w", l.server, err)
		default:
			retry = err
		}
	} else {
		p = nil // none, or for another server: replaced below
	}
	if p == nil {
		if p, err = l.start(ctx); err != nil {
			return err
		}
	}

	if noWait {
		if l.g.json {
			return l.g.out().json(pendingCode{UserCode: p.UserCode, VerificationURI: p.VerificationURI, ExpiresAt: p.ExpiresAt, Server: p.Server})
		}
		return l.say(l.instructions(p) + fmt.Sprintf("After approving, run %q to finish.\n", l.again()))
	}
	if err := l.say(l.instructions(p) + fmt.Sprintf("Waiting for approval... (Ctrl-C stops waiting; %q resumes)\n", l.again())); err != nil {
		return err
	}
	if retry != nil {
		l.note(p, retry)
	}
	return l.wait(ctx, p)
}

// start asks the server for a new device login and saves it as pending.
func (l *deviceLogin) start(ctx context.Context) (*pendingLogin, error) {
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	d, err := l.c.StartDeviceLogin(ctx, deviceClient(l.g.app.Version, host))
	if err != nil {
		return nil, fmt.Errorf("start a device login with %s: %w", l.server, err)
	}
	p := &pendingLogin{
		Server:          l.server,
		DeviceCode:      d.DeviceCode,
		UserCode:        d.UserCode,
		VerificationURI: d.VerificationUri,
		ExpiresAt:       l.g.app.Now().Add(time.Duration(d.ExpiresIn) * time.Second).Truncate(time.Second),
		Interval:        d.Interval,
	}
	if err := savePending(l.pendingPath, p); err != nil {
		return nil, fmt.Errorf("save the pending login: %w", err)
	}
	return p, nil
}

// wait polls every interval until the login is approved, denied or expired.
// Network errors, timeouts and 5xx are retried. A cancelled ctx keeps the
// pending login for the next "charlie login".
func (l *deviceLogin) wait(ctx context.Context, p *pendingLogin) error {
	interval := time.Duration(max(p.Interval, 1)) * time.Second
	// By then the server has ended the device login even if it was approved
	// at the last moment; this only guards against waiting forever.
	giveUp := p.ExpiresAt.Add(approvalGrace)
	for {
		if l.g.app.Sleep(ctx, interval) != nil {
			return l.interrupted()
		}
		if l.g.app.Now().After(giveUp) {
			l.forget()
			return l.expired(p)
		}
		s, err := l.poll(ctx, p)
		switch code := client.ErrorCode(err); {
		case err == nil:
			return l.finish(s)
		case code == "authorization_pending":
		case code == "access_denied":
			l.forget()
			return errDenied
		case code == "expired_token":
			l.forget()
			return l.expired(p)
		case ctx.Err() != nil:
			return l.interrupted()
		case transient(err):
			l.note(p, err)
		default:
			return fmt.Errorf("wait for the approval: %w", err)
		}
	}
}

func (l *deviceLogin) poll(ctx context.Context, p *pendingLogin) (*apiv1.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return l.c.PollDeviceLogin(ctx, p.DeviceCode)
}

// finish saves the token and forgets the pending login.
func (l *deviceLogin) finish(s *apiv1.Session) error {
	if err := saveConfig(l.configPath, config{Server: l.server, Token: s.Token, ExpiresAt: &s.ExpiresAt}); err != nil {
		return fmt.Errorf("save the login: %w", err)
	}
	l.forget()
	return show(l.g, &loginResult{Principal: s.Principal, ExpiresAt: &s.ExpiresAt}, func(r *loginResult) string {
		return fmt.Sprintf("Logged in to %s as %s (%s) until %s.\nSaved to %s\n",
			l.server, r.Name, r.Role, stamp(*r.ExpiresAt), l.configPath)
	})
}

// instructions tell the person what to do with the code.
func (l *deviceLogin) instructions(p *pendingLogin) string {
	return fmt.Sprintf("Open %s, log in as an administrator and enter this code:\n\n    %s\n\n"+
		"The code expires at %s (%s). The administrator picks the role this CLI gets.\n",
		p.VerificationURI, p.UserCode, p.ExpiresAt.Local().Format("15:04"), inMinutes(p.ExpiresAt.Sub(l.g.app.Now())))
}

// say prints instructions: to stdout, or to stderr when stdout carries JSON.
func (l *deviceLogin) say(s string) error {
	if l.g.json {
		_, err := fmt.Fprint(l.g.app.Stderr, s)
		return err
	}
	return l.g.out().text(s)
}

// note tells once that polling hit a network or server error.
func (l *deviceLogin) note(p *pendingLogin, err error) {
	if l.noted {
		return
	}
	l.noted = true
	fmt.Fprintf(l.g.app.Stderr, "cannot reach %s (%v); retrying every %ds\n", l.server, err, max(p.Interval, 1))
}

// again is the command that resumes this login.
func (l *deviceLogin) again() string {
	if l.g.server != "" {
		return "charlie login --server " + l.server
	}
	return "charlie login"
}

func (l *deviceLogin) forget() { _, _ = removeFile(l.pendingPath) }

func (l *deviceLogin) expired(p *pendingLogin) error {
	return fmt.Errorf("the code %s expired; %q gets a new one", p.UserCode, l.again())
}

func (l *deviceLogin) interrupted() error {
	return &exitError{code: ExitInterrupted, msg: fmt.Sprintf(
		"interrupted; the login is still pending and %q resumes it with the same code", l.again())}
}

// transient reports whether asking again may work: network errors and
// timeouts, 5xx, 408 and 429.
func transient(err error) bool {
	var ae *client.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500 || ae.Status == http.StatusRequestTimeout || ae.Status == http.StatusTooManyRequests
	}
	return true
}

// deviceClient tells the approving administrator what is asking, e.g.
// "charlie v0.5.0 on rin-sbx-12". The server takes up to 200 characters and
// no control or formatting characters.
func deviceClient(version, host string) string {
	clean := func(s string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, s))
	}
	host = cmp.Or(clean(host), "unknown")
	s := []rune("charlie " + clean(version) + " on " + host)
	if len(s) > maxClientChars {
		s = s[:maxClientChars]
	}
	return strings.TrimSpace(string(s))
}

// inMinutes says how far away d is, e.g. "in 10 minutes".
func inMinutes(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case d < time.Minute:
		return "in less than a minute"
	case m == 1:
		return "in 1 minute"
	}
	return fmt.Sprintf("in %d minutes", m)
}

func (g *globals) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "End the saved login on the server and remove it",
		Long: `Asks the server to end the saved token, then removes config.json and any
pending login. When the server cannot be reached, the token stays valid on
the server until it expires.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := g.app.configDir()
			if err != nil {
				return err
			}
			path, pending := filepath.Join(dir, configFile), filepath.Join(dir, pendingFile)
			file, _ := loadConfig(path) // a damaged file is removed all the same

			var out string
			if file.Token != "" {
				out = fmt.Sprintf("Logged out of %s. %s\n", file.Server, g.endToken(cmd.Context(), file))
			} else {
				out = "Not logged in.\n"
			}
			for _, f := range []string{path, pending} {
				removed, err := removeFile(f)
				if err != nil {
					return err
				}
				if removed {
					out += "Removed " + f + "\n"
				}
			}
			return g.out().text(out)
		},
	}
}

// endToken asks the server to end the saved token and says how that went.
func (g *globals) endToken(ctx context.Context, file config) string {
	ctx, cancel := context.WithTimeout(ctx, logoutTimeout)
	defer cancel()
	err := g.newClient(file.Server, file.Token).Logout(ctx)
	var ae *client.APIError
	until := "it expires"
	if file.ExpiresAt != nil {
		until = stamp(*file.ExpiresAt)
	}
	switch {
	case err == nil:
		return "The server ended the token."
	case unauthorized(err):
		return "The token had already expired or been revoked."
	case errors.As(err, &ae):
		return fmt.Sprintf("The server did not end the token (%v); it stays valid until %s.", err, until)
	}
	return fmt.Sprintf("Could not reach the server (%v); the token stays valid until %s.", err, until)
}

func (g *globals) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the token belongs to",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.settings(cmd)
			if err != nil {
				return err
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			me, err := c.Me(ctx)
			if err != nil {
				return err
			}
			return show(g, &loginResult{Principal: *me, ExpiresAt: s.expiresAt}, func(r *loginResult) string {
				out := fmt.Sprintf("%s (%s) on %s\n", r.Name, r.Role, s.server)
				if r.ExpiresAt != nil {
					out += fmt.Sprintf("The saved login expires at %s.\n", stamp(*r.ExpiresAt))
				}
				return out
			})
		},
	}
}
