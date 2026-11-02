package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (g *globals) loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login --server URL",
		Short: "Check a token with the server and save the login",
		Long: `Reads the token from stdin (without echo on a terminal), checks it with
GET /v1/me and saves the server and token to the user config directory
(charlie/config.json, mode 0600).

  charlie login --server http://localhost:8080
  echo "$TOKEN" | charlie login --server https://platform.example.com`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.settings(cmd)
			if err != nil {
				return err
			}
			if s.server == "" {
				return usagef(cmd, "--server is required")
			}
			token, err := g.readToken()
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			me, err := g.newClient(s.server, token).Me(ctx)
			if err != nil {
				return fmt.Errorf("check the token with %s: %w", s.server, err)
			}
			if err := saveConfig(s.path, config{Server: s.server, Token: token}); err != nil {
				return fmt.Errorf("save the login: %w", err)
			}
			return show(g, me, func(p *apiv1.Principal) string {
				return fmt.Sprintf("Logged in to %s as %s (%s).\nSaved to %s\n", s.server, p.Name, p.Role, s.path)
			})
		},
	}
}

// readToken prompts without echo on a terminal and reads one line otherwise.
func (g *globals) readToken() (string, error) {
	var token string
	if f, ok := g.app.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(g.app.Stderr, "Token: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(g.app.Stderr)
		if err != nil {
			return "", fmt.Errorf("read the token: %w", err)
		}
		token = string(b)
	} else {
		line, err := bufio.NewReader(g.app.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read the token from stdin: %w", err)
		}
		token = line
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("no token given on stdin")
	}
	return token, nil
}

func (g *globals) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the saved login",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := configPath(g.app.ConfigDir)
			if err != nil {
				return err
			}
			removed, err := removeConfig(path)
			if err != nil {
				return err
			}
			p := g.out()
			if removed {
				return p.text("Logged out; removed " + path + "\n")
			}
			return p.text("Not logged in.\n")
		},
	}
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
			return show(g, me, func(p *apiv1.Principal) string {
				return fmt.Sprintf("%s (%s) on %s\n", p.Name, p.Role, s.server)
			})
		},
	}
}
