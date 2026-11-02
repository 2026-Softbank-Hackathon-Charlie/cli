package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/cli/internal/update"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/spf13/cobra"
)

func (g *globals) matrixCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "matrix",
		Short: "Show the latest finished run for every pair of environments",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			m, err := c.GetMatrix(ctx)
			if err != nil {
				return err
			}
			return show(g, m, renderMatrix)
		},
	}
}

func renderMatrix(m *apiv1.Matrix) string {
	rows := [][]string{{"SOURCE", "TARGET", "STATUS", "FINISHED", "DOWNTIME", "FAILED REQUESTS", "MISSING ROWS", "MISSING WRITES", "MISSING FILES", "RUN"}}
	for _, rt := range m.Routes {
		if rt.RunId == nil {
			rows = append(rows, []string{rt.Source, rt.Target, "[ ] never run", "", "", "", "", "", "", ""})
			continue
		}
		status := ""
		if rt.Status != nil {
			status = runMark(*rt.Status) + " " + string(*rt.Status)
		}
		var res apiv1.RunResult
		if rt.Result != nil {
			res = *rt.Result
		}
		rows = append(rows, []string{rt.Source, rt.Target, status, stampPtr(rt.FinishedAt), millis(res.DowntimeMillis),
			count(res.FailedRequests), count(res.MissingRows), count(res.MissingWrites), count(res.MissingFiles), *rt.RunId})
	}
	return "Environments: " + strings.Join(m.Environments, ", ") + "\n\n" + table(rows)
}

func (g *globals) auditCmd() *cobra.Command {
	var (
		limit  int
		before int64
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show state-changing and denied requests, newest first",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("limit") && (limit < 1 || limit > 500) {
				return usagef(cmd, "--limit must be between 1 and 500")
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListAudit(ctx, before, limit)
			if err != nil {
				return err
			}
			return show(g, l, renderAudit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "entries per page, 1 to 500 (server default 100)")
	cmd.Flags().Int64Var(&before, "before", 0, "show entries with an id lower than this (next page)")
	return cmd
}

func renderAudit(l *apiv1.AuditList) string {
	rows := [][]string{{"ID", "TIME", "ACTOR", "ROLE", "STATUS", "ACTION", "TARGET"}}
	for _, e := range l.Entries {
		rows = append(rows, []string{fmt.Sprint(e.Id), stamp(e.Time), e.Actor, e.Role, fmt.Sprint(e.Status), e.Action, e.Target})
	}
	out := table(rows)
	if l.NextBefore != nil {
		out += fmt.Sprintf("\nOlder entries: charlie audit --before %d\n", *l.NextBefore)
	}
	return out
}

func (g *globals) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the charlie version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := g.out()
			if g.json {
				return p.json(map[string]string{"version": g.app.Version})
			}
			return p.text("charlie " + g.app.Version + "\n")
		},
	}
}

func (g *globals) updateCmd() *cobra.Command {
	var background bool
	c := &cobra.Command{
		Use:   "update",
		Short: "Install the newest charlie now",
		Long: `charlie updates itself in the background after a command ends, at most
once every 10 minutes (CHARLIE_UPDATE_INTERVAL changes it, CHARLIE_NO_UPDATE=1
turns it off). This command does the same check now and waits for it.

Updating needs the go command and access to the private repositories
(GOPRIVATE=github.com/2026-Softbank-Hackathon-Charlie, git credentials for GitHub).`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.app.Updater == nil {
				return errors.New("updates are not available in this build")
			}
			cfg := *g.app.Updater
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			out := cmd.OutOrStdout()
			if background {
				// Started by MaybeStart with stdout going to update.log.
				fmt.Fprintf(out, "%s background update\n", time.Now().Format(time.RFC3339))
				_, err := update.Update(ctx, cfg, out)
				if err != nil && !errors.Is(err, update.ErrBusy) {
					fmt.Fprintf(out, "update failed: %v\n", err)
				}
				return nil
			}
			res, err := update.Update(ctx, cfg, io.Discard)
			if err != nil {
				return err
			}
			if !res.Updated {
				return g.out().text(fmt.Sprintf("charlie %s is up to date\n", res.From))
			}
			_ = update.TakeNotice(cfg) // reported here; do not repeat it on the next command
			return g.out().text(fmt.Sprintf("Updated charlie from %s to %s.\n", res.From, res.To))
		},
	}
	c.Flags().BoolVar(&background, "background", false, "run quietly and log to update.log (used by the automatic update)")
	_ = c.Flags().MarkHidden("background")
	return c
}
