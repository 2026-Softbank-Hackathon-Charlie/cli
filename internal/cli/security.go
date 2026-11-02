package cli

import (
	"fmt"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

func (g *globals) securityCmd() *cobra.Command {
	var (
		environment, severity string
		refresh               bool
	)
	cmd := &cobra.Command{
		Use:   "security",
		Short: "Show the security and governance state of every environment",
		Long: `Shows findings from every environment in one view: the Kubernetes clusters,
the AWS account (IAM, EC2, CloudTrail, GuardDuty, Security Hub, S3) and the
platform's own audit log and settings, most severe first and grouped by
environment, followed by each source and whether it could be read.

The server caches the view for 60 seconds; --refresh reads the sources again
(at most once every 10 seconds).`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sev := apiv1.SecuritySeverity(severity)
			if severity != "" && !sev.Valid() {
				return usagef(cmd, "--severity must be critical, high, medium, low or info")
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			rep, err := c.GetSecurity(ctx, client.SecurityOptions{Environment: environment, Severity: sev, Refresh: refresh})
			if err != nil {
				return err
			}
			return show(g, rep, renderSecurity)
		},
	}
	f := cmd.Flags()
	f.StringVar(&environment, "environment", "", "only this environment (e.g. gke, aws-account, platform)")
	f.StringVar(&severity, "severity", "", "minimum severity: critical, high, medium, low or info")
	f.BoolVar(&refresh, "refresh", false, "read the sources again instead of the server's cached view")
	return cmd
}

func sourceMark(s apiv1.SecuritySourceStatus) string {
	switch s {
	case apiv1.SecuritySourceStatusOk:
		return "[ok]"
	case apiv1.SecuritySourceStatusNotConfigured:
		return "[ ]"
	}
	return "[x]"
}

func severityCounts(c apiv1.SeverityCounts) string {
	return fmt.Sprintf("%d critical, %d high, %d medium, %d low, %d info", c.Critical, c.High, c.Medium, c.Low, c.Info)
}

func renderSecurity(r *apiv1.SecurityReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Security view at %s: %d findings (%s)\n", stamp(r.GeneratedAt), r.Summary.BySeverity.Total, severityCounts(r.Summary.BySeverity))

	byEnv := map[string][]apiv1.SecurityFinding{}
	var envs []string
	for _, s := range r.Sources {
		if _, ok := byEnv[s.Environment]; !ok {
			byEnv[s.Environment] = nil
			envs = append(envs, s.Environment)
		}
	}
	for _, f := range r.Findings {
		if _, ok := byEnv[f.Environment]; !ok {
			envs = append(envs, f.Environment)
		}
		byEnv[f.Environment] = append(byEnv[f.Environment], f)
	}
	for _, env := range envs {
		list := byEnv[env]
		if len(list) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s: %s\n", env, severityCounts(r.Summary.ByEnvironment[env]))
		rows := [][]string{{"SEVERITY", "CATEGORY", "SOURCE", "TITLE", "RESOURCE"}}
		for _, f := range list {
			rows = append(rows, []string{string(f.Severity), string(f.Category), f.Source, clip(f.Title, 80), f.Resource})
		}
		b.WriteString(table(rows))
	}
	if len(r.Findings) == 0 {
		b.WriteString("\nNo findings.\n")
	}

	b.WriteString("\n")
	rows := [][]string{{"SOURCE", "PROVIDER", "ENVIRONMENT", "STATUS", "FINDINGS", "CHECKED", "MESSAGE"}}
	for _, s := range r.Sources {
		rows = append(rows, []string{s.Name, string(s.Provider), s.Environment, sourceMark(s.Status) + " " + string(s.Status),
			fmt.Sprint(s.Findings), clock(s.CheckedAt), str(s.Message)})
	}
	b.WriteString(table(rows))
	return b.String()
}

// clip keeps s on one line and at most n runes, so one long value does not
// push the columns after it to the right.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}
