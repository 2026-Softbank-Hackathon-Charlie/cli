package cli

import (
	"fmt"
	"strings"

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
