package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/spf13/cobra"
)

func (g *globals) planCmd() *cobra.Command {
	var file string
	submit := &cobra.Command{
		Use:   "submit -f FILE",
		Short: "Submit an engine plan JSON for approval (-f - reads stdin)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return usagef(cmd, "-f FILE is required (use -f - for stdin)")
			}
			raw, err := g.readFile(file)
			if err != nil {
				return err
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			rec, err := c.SubmitPlan(ctx, raw)
			if err != nil {
				return err
			}
			return show(g, rec, func(p *apiv1.PlanRecord) string {
				return fmt.Sprintf("Submitted plan %s.\n\n%s\nApprove it with: charlie plan approve %s\n", p.Ref, renderPlan(p), p.Ref)
			})
		},
	}
	submit.Flags().StringVarP(&file, "file", "f", "", "plan JSON file, or - for stdin")

	list := &cobra.Command{
		Use:   "list",
		Short: "List plans, newest first",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListPlans(ctx)
			if err != nil {
				return err
			}
			return show(g, l, renderPlanList)
		},
	}

	showCmd := &cobra.Command{
		Use:   "show REF",
		Short: "Show one plan",
		Args:  exactArgs("REF"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			p, err := c.GetPlan(ctx, args[0])
			if err != nil {
				return err
			}
			return show(g, p, renderPlan)
		},
	}

	var approveComment string
	approve := &cobra.Command{
		Use:   "approve REF",
		Short: "Approve a pending plan (role operator)",
		Args:  exactArgs("REF"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			p, err := c.ApprovePlan(ctx, args[0], approveComment)
			if err != nil {
				return err
			}
			return show(g, p, func(p *apiv1.PlanRecord) string {
				return fmt.Sprintf("Approved plan %s.\n\n%s\nStart it with: charlie migrate start %s\n", p.Ref, renderPlan(p), p.Ref)
			})
		},
	}
	approve.Flags().StringVar(&approveComment, "comment", "", "review comment")

	var rejectComment string
	reject := &cobra.Command{
		Use:   "reject REF --comment TEXT",
		Short: "Reject a pending plan (role operator)",
		Args:  exactArgs("REF"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if rejectComment == "" {
				return usagef(cmd, "--comment is required: say why the plan is rejected")
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			p, err := c.RejectPlan(ctx, args[0], rejectComment)
			if err != nil {
				return err
			}
			return show(g, p, func(p *apiv1.PlanRecord) string {
				return fmt.Sprintf("Rejected plan %s.\n\n%s", p.Ref, renderPlan(p))
			})
		},
	}
	reject.Flags().StringVar(&rejectComment, "comment", "", "why the plan is rejected (required)")

	return group("plan", "Migration plans and their human approval", submit, list, showCmd, approve, reject)
}

func (g *globals) readFile(name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(g.app.Stdin)
	}
	return os.ReadFile(name)
}

func renderPlanList(l *apiv1.PlanList) string {
	rows := [][]string{{"REF", "STATUS", "ROUTE", "PLAN ID", "FILES", "GATEWAY", "SUBMITTED", "BY", "RUN"}}
	for _, p := range l.Plans {
		s := p.Summary
		rows = append(rows, []string{p.Ref, planMark(p.Status) + " " + string(p.Status), route(s.Source.Env, s.Target.Env),
			s.PlanId, yesNo(s.Files), s.GatewayMode, stamp(p.SubmittedAt), p.SubmittedBy, str(p.RunId)})
	}
	return table(rows)
}

func endpoint(e apiv1.PlanEndpoint) string {
	s := fmt.Sprintf("%s (namespace %s, cluster %s", e.Env, e.Namespace, e.Cluster)
	if e.Context != nil {
		s += ", context " + *e.Context
	}
	return s + ")"
}

func renderPlan(p *apiv1.PlanRecord) string {
	s := p.Summary
	pairs := [][2]string{
		{"Ref", p.Ref},
		{"Status", planMark(p.Status) + " " + string(p.Status)},
		{"Plan id", s.PlanId},
		{"Source", endpoint(s.Source)},
		{"Target", endpoint(s.Target)},
		{"Moves files", yesNo(s.Files)},
		{"Gateway mode", s.GatewayMode},
		{"Fence gate", yesNo(s.FenceGate)},
		{"Max downtime", s.MaxDowntime},
		{"Submitted", stamp(p.SubmittedAt) + " by " + p.SubmittedBy},
	}
	if p.ReviewedBy != nil {
		pairs = append(pairs, [2]string{"Reviewed", stampPtr(p.ReviewedAt) + " by " + *p.ReviewedBy})
	}
	if p.ReviewComment != nil {
		pairs = append(pairs, [2]string{"Comment", *p.ReviewComment})
	}
	if p.RunId != nil {
		pairs = append(pairs, [2]string{"Run", *p.RunId})
	}
	return fields(pairs)
}
