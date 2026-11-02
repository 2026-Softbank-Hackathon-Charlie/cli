package cli

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

func (g *globals) migrateCmd() *cobra.Command {
	return group("migrate", "Start, watch and steer migration runs",
		g.migrateStartCmd(), g.migrateListCmd(), g.migrateStatusCmd(), g.migrateWatchCmd(),
		g.migrateSwitchCmd(), g.migrateAbortCmd(), g.migrateResumeCmd())
}

func (g *globals) migrateStartCmd() *cobra.Command {
	var (
		autoSwitch     bool
		weights        []int
		step           time.Duration
		maxErr         float64
		maxP95, minReq int
		observe        time.Duration
		observeStep    time.Duration
		autoRollback   bool
		key            string
		watch          bool
	)
	cmd := &cobra.Command{
		Use:   "start PLANREF",
		Short: "Start an approved plan (role operator)",
		Long: `Starts an approved plan and prints the run id. The run syncs data, can send a
share of read traffic to the target first (--canary), and then waits in
awaiting_switch for "charlie migrate switch" unless --auto-switch is given.
With --observe it keeps watching the target against the same gate after the
cutover; --auto-rollback then starts the rollback by itself on a breach.

An idempotency key is generated unless --idempotency-key is given. Pass the
printed key again to retry without starting a second run.`,
		Args: exactArgs("PLANREF"),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := apiv1.StartMigrationRequest{PlanRef: args[0]}
			if cmd.Flags().Changed("auto-switch") {
				req.AutoSwitch = &autoSwitch
			}
			canary, obs, err := runOptions(cmd, runFlags{weights, step, observe, observeStep, autoRollback, maxErr, maxP95, minReq})
			if err != nil {
				return err
			}
			req.Canary, req.Observe = canary, obs
			if key == "" {
				key = "charlie-" + rand.Text()
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			resp, err := c.StartMigration(ctx, req, key)
			if err != nil {
				return err
			}
			p := g.out()
			switch {
			case g.json && watch:
				fmt.Fprintf(g.app.Stderr, "idempotency key: %s\n", key)
				err = p.jsonLine(resp)
			case g.json:
				fmt.Fprintf(g.app.Stderr, "idempotency key: %s\n", key)
				err = p.json(resp)
			default:
				msg := fmt.Sprintf("Started run %s for plan %s.\nIdempotency key: %s (pass --idempotency-key to retry safely)\n", resp.RunId, resp.PlanRef, key)
				if !watch {
					msg += "Watch it with: charlie migrate watch " + resp.RunId + "\n"
				}
				err = p.text(msg)
			}
			if err != nil || !watch {
				return err
			}
			return g.watch(cmd, c, resp.RunId)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&autoSwitch, "auto-switch", false, "cut over as soon as data is in sync, without waiting for switch")
	f.IntSliceVar(&weights, "canary", nil, "shares of read traffic to send to the target before cutover, e.g. 10,50")
	f.DurationVar(&step, "canary-step", 20*time.Second, "length of each canary step")
	f.DurationVar(&observe, "observe", 0, "after cutover, check the target against the gate this long before the run ends, e.g. 2m")
	f.DurationVar(&observeStep, "observe-step", 20*time.Second, "length of each observe step")
	f.BoolVar(&autoRollback, "auto-rollback", false, "with --observe, start the rollback by itself when the target breaks the gate")
	f.Float64Var(&maxErr, "max-error-rate", 0.01, "gate (canary and observe): highest error rate on the target")
	f.IntVar(&maxP95, "max-p95", 800, "gate (canary and observe): highest p95 latency on the target, in milliseconds")
	f.IntVar(&minReq, "min-requests", 20, "gate (canary and observe): fewest target requests per step")
	f.StringVar(&key, "idempotency-key", "", "key that makes a retry return the original run")
	f.BoolVar(&watch, "watch", false, "watch the run after starting it")
	return cmd
}

// runFlags are the "migrate start" flags that shape the canary and the observe phase.
type runFlags struct {
	weights        []int
	step           time.Duration
	observe        time.Duration
	observeStep    time.Duration
	autoRollback   bool
	maxErr         float64
	maxP95, minReq int
}

// runOptions builds the canary and observe options. Both use the same gate
// flags. Only what the user set is sent, so the server's defaults apply to
// the rest.
func runOptions(cmd *cobra.Command, fl runFlags) (*apiv1.CanaryOptions, *apiv1.ObserveOptions, error) {
	f := cmd.Flags()
	canaryOn, observeOn := f.Changed("canary"), f.Changed("observe")
	if !canaryOn && f.Changed("canary-step") {
		return nil, nil, usagef(cmd, "--canary-step needs --canary")
	}
	if !observeOn {
		for _, n := range []string{"observe-step", "auto-rollback"} {
			if f.Changed(n) {
				return nil, nil, usagef(cmd, "--%s needs --observe", n)
			}
		}
	}
	if !canaryOn && !observeOn {
		for _, n := range []string{"max-error-rate", "max-p95", "min-requests"} {
			if f.Changed(n) {
				return nil, nil, usagef(cmd, "--%s needs --canary or --observe", n)
			}
		}
		return nil, nil, nil
	}

	var gate *apiv1.GateThresholds
	var g apiv1.GateThresholds
	if f.Changed("max-error-rate") {
		v := float32(fl.maxErr)
		g.MaxErrorRate = &v
	}
	if f.Changed("max-p95") {
		g.MaxP95Millis = &fl.maxP95
	}
	if f.Changed("min-requests") {
		g.MinRequests = &fl.minReq
	}
	if g != (apiv1.GateThresholds{}) {
		gate = &g
	}

	var c *apiv1.CanaryOptions
	if canaryOn {
		if len(fl.weights) == 0 {
			return nil, nil, usagef(cmd, "--canary needs at least one weight, e.g. --canary 10,50")
		}
		for _, w := range fl.weights {
			if w < 1 || w > 100 {
				return nil, nil, usagef(cmd, "--canary weights must be between 1 and 100, got %d", w)
			}
		}
		c = &apiv1.CanaryOptions{Weights: fl.weights, Gate: gate}
		if f.Changed("canary-step") {
			if fl.step <= 0 {
				return nil, nil, usagef(cmd, "--canary-step must be positive")
			}
			s := fl.step.String()
			c.StepDuration = &s
		}
	}

	var o *apiv1.ObserveOptions
	if observeOn {
		if fl.observe <= 0 {
			return nil, nil, usagef(cmd, "--observe must be positive, e.g. --observe 2m")
		}
		d := fl.observe.String()
		o = &apiv1.ObserveOptions{Duration: &d, Gate: gate}
		if f.Changed("observe-step") {
			if fl.observeStep <= 0 {
				return nil, nil, usagef(cmd, "--observe-step must be positive")
			}
			s := fl.observeStep.String()
			o.StepDuration = &s
		}
		if f.Changed("auto-rollback") {
			o.AutoRollback = &fl.autoRollback
		}
	}
	return c, o, nil
}

func (g *globals) migrateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List runs, newest first",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListMigrations(ctx)
			if err != nil {
				return err
			}
			return show(g, l, func(l *apiv1.RunList) string { return renderRunList(l, time.Now()) })
		},
	}
}

func renderRunList(l *apiv1.RunList, now time.Time) string {
	rows := [][]string{{"RUN", "STATUS", "PHASE", "ROUTE", "PLAN", "STARTED", "BY", "DURATION", "DOWNTIME"}}
	for _, r := range l.Runs {
		rows = append(rows, []string{r.RunId, runMark(r.Status) + " " + string(r.Status), string(r.Phase),
			route(r.Source.Env, r.Target.Env), r.PlanRef, stamp(r.StartedAt), r.StartedBy,
			human(runDuration(&r, now)), millis(downtime(&r))})
	}
	return table(rows)
}

func runDuration(r *apiv1.Run, now time.Time) time.Duration {
	switch {
	case r.Result.DurationMillis != nil:
		return time.Duration(*r.Result.DurationMillis) * time.Millisecond
	case r.EndedAt != nil:
		return r.EndedAt.Sub(r.StartedAt)
	}
	return now.Sub(r.StartedAt)
}

func downtime(r *apiv1.Run) *int64 {
	if r.Result.DowntimeMillis != nil {
		return r.Result.DowntimeMillis
	}
	return r.Engine.DowntimeMillis
}

func (g *globals) migrateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status RUNID",
		Short: "Show a run's steps, numbers and canary once",
		Args:  exactArgs("RUNID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			r, err := c.GetMigration(ctx, args[0])
			if err != nil {
				return err
			}
			return show(g, r, func(r *apiv1.Run) string {
				v := newRunView()
				v.snapshot(r)
				return v.frame(time.Now(), 0, 0)
			})
		},
	}
}

func (g *globals) migrateWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch RUNID",
		Short: "Follow a run live until it ends",
		Long: `Follows a run until it ends, reconnecting without losing or repeating events.
On a terminal the view redraws in place; otherwise it prints one line per event.
Exits 0 when the run succeeded and 3 when it ended any other way.`,
		Args: exactArgs("RUNID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			return g.watch(cmd, c, args[0])
		},
	}
}

func (g *globals) migrateSwitchCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "switch RUNID",
		Short: "Approve the cutover of a run in awaiting_switch (role operator)",
		Args:  exactArgs("RUNID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAction(cmd, args[0], watch, func(ctx context.Context, c *client.Client) (*apiv1.Run, error) {
				return c.SwitchMigration(ctx, args[0])
			}, "Cutover approved for run %s: the engine holds writes, promotes the target and moves traffic.\n")
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "watch the run after switching")
	return cmd
}

func (g *globals) migrateAbortCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "abort RUNID",
		Short: "Stop a running migration (role operator)",
		Long: `Stops a running migration. Where the engine is decides the result: before
writes are held nothing changed (aborted); before the target is asked to promote
it rolls back to the source (rolled_back); after that it halts for an operator.`,
		Args: exactArgs("RUNID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAction(cmd, args[0], false, func(ctx context.Context, c *client.Client) (*apiv1.Run, error) {
				return c.AbortMigration(ctx, args[0], reason)
			}, "Abort requested for run %s; the engine decides the final status.\n")
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the run is stopped (kept in the run and the audit log)")
	return cmd
}

func (g *globals) migrateResumeCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "resume RUNID",
		Short: "Continue an interrupted, failed or aborted run (role operator)",
		Args:  exactArgs("RUNID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAction(cmd, args[0], watch, func(ctx context.Context, c *client.Client) (*apiv1.Run, error) {
				return c.ResumeMigration(ctx, args[0])
			}, "Resumed run %s; steps that already succeeded are skipped.\n")
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "watch the run after resuming")
	return cmd
}

// runAction performs one state change on a run, prints it and optionally
// watches the run afterwards.
func (g *globals) runAction(cmd *cobra.Command, runID string, watch bool, do func(context.Context, *client.Client) (*apiv1.Run, error), done string) error {
	c, err := g.client(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := callCtx(cmd)
	defer cancel()
	r, err := do(ctx, c)
	if err != nil {
		return err
	}
	p := g.out()
	switch {
	case g.json && watch:
		err = p.jsonLine(r)
	case g.json:
		err = p.json(r)
	default:
		msg := fmt.Sprintf(done, r.RunId)
		if !watch {
			msg += "Watch it with: charlie migrate watch " + r.RunId + "\n"
		}
		err = p.text(msg)
	}
	if err != nil || !watch {
		return err
	}
	return g.watch(cmd, c, runID)
}
