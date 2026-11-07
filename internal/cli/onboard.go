package cli

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

func (g *globals) onboardCmd() *cobra.Command {
	return group("onboard", "Bring an app that runs with docker compose on a VM onto the platform (ADR 0042)",
		g.onboardStartCmd(), g.onboardListCmd(), g.onboardStatusCmd(), g.onboardApproveCmd(),
		g.onboardCutoverCmd(), g.onboardDecommissionCmd(), g.onboardResumeCmd(), g.onboardDeleteCmd())
}

func (g *globals) onboardStartCmd() *cobra.Command {
	var repo, url, env string
	var follow bool
	cmd := &cobra.Command{
		Use:   "start --repo OWNER/NAME --url https://LABEL.hopcharlie.dev [--env ENV]",
		Short: "Start onboarding an app that runs outside the platform (role operator)",
		Long: `Starts onboarding the app in --repo that answers at --url now, with docker
compose on one VM. The label of --url is the app's name. The platform reads
GitHub, DNS, the cloud, the VM and its database (discovering), has hopai
write a contract branch and opens a PR (patching), deploys the PR branch as a
temporary app (checking) and waits for your approval (awaiting_approval).
Nothing on the source VM or database changes before "charlie onboard approve".

Without --env the target is the environment of the VM's cloud (eks for AWS,
gke for GCP). With --follow, follows the onboarding like "charlie onboard
status --follow".`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" || url == "" {
				return usagef(cmd, "--repo and --url are required")
			}
			if !repoName(repo) {
				return usagef(cmd, "--repo must be OWNER/NAME, got %q", repo)
			}
			if !strings.HasPrefix(url, "https://") {
				return usagef(cmd, "--url must be https://LABEL.<app domain>, got %q", url)
			}
			req := apiv1.OnboardingRequest{Repo: repo, ServiceUrl: url}
			if env != "" {
				req.TargetEnv = &env
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			o, err := c.StartOnboarding(ctx, req)
			if err != nil {
				return onboardError(err, "")
			}
			p := g.out()
			switch {
			case g.json && follow:
				err = p.jsonLine(o)
			case g.json:
				err = p.json(o)
			default:
				msg := fmt.Sprintf("Started onboarding %s of app %s from %s.\n", o.Id, o.App, o.Repo)
				if !follow {
					msg += "Follow it with: charlie onboard status " + o.Id + " --follow\n"
				}
				err = p.text(msg)
			}
			if err != nil || !follow {
				return err
			}
			return g.followOnboarding(cmd, c, o.Id)
		},
	}
	f := cmd.Flags()
	f.StringVar(&repo, "repo", "", "the app's GitHub repository, OWNER/NAME")
	f.StringVar(&url, "url", "", "where the app answers now, https://LABEL.<app domain>; LABEL is the app's name")
	f.StringVar(&env, "env", "", "the environment to move it to (default the VM's cloud's)")
	f.BoolVar(&follow, "follow", false, "follow the onboarding after starting it")
	return cmd
}

func (g *globals) onboardListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List onboardings, newest first, done ones included",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListOnboardings(ctx)
			if err != nil {
				return onboardError(err, "")
			}
			return show(g, l, renderOnboardingList)
		},
	}
}

func renderOnboardingList(l *apiv1.OnboardingList) string {
	if len(l.Onboardings) == 0 {
		return "No onboardings. Start one with: charlie onboard start --repo OWNER/NAME --url https://LABEL.hopcharlie.dev\n"
	}
	rows := [][]string{{"ID", "APP", "REPO", "TARGET", "STATE", "PR", "UPDATED"}}
	for i := range l.Onboardings {
		o := &l.Onboardings[i]
		pr := ""
		if o.Pr != nil {
			pr = fmt.Sprintf("#%d", o.Pr.Number)
		}
		rows = append(rows, []string{o.Id, o.App, o.Repo, o.TargetEnv, onboardStateText(o), pr, stamp(o.UpdatedAt)})
	}
	return table(rows)
}

// onboardStateText is the state with what failed and a running deletion.
func onboardStateText(o *apiv1.Onboarding) string {
	s := string(o.State)
	if o.State == apiv1.OnboardingStateFailed && o.FailedState != nil {
		s += " (in " + string(*o.FailedState) + ")"
	}
	if o.Deleting {
		s += ", deleting"
	}
	return s
}

func (g *globals) onboardStatusCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "status ID [--follow]",
		Short: "Show an onboarding: state, facts, report, PR, test deployment, what a person has to do",
		Long: `Shows an onboarding and the next command for its state. With --follow,
prints its steps as they happen until it is done or deleted; a failed
onboarding is followed on, since "charlie onboard resume" carries it on.
With --json and --follow, prints one {"event", "id", "data"} line per
message.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if follow {
				return g.followOnboarding(cmd, c, args[0])
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			o, err := c.GetOnboarding(ctx, args[0])
			if err != nil {
				return onboardError(err, args[0])
			}
			return show(g, o, renderOnboarding)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the onboarding's steps")
	return cmd
}

func renderOnboarding(o *apiv1.Onboarding) string {
	var b strings.Builder
	rows := [][2]string{
		{"Onboarding", o.Id},
		{"App", o.App},
		{"Repo", o.Repo},
		{"Service URL", o.ServiceUrl},
		{"Target env", cmp.Or(o.TargetEnv, "(chosen by discovery)")},
		{"State", onboardStateText(o)},
	}
	if f := str(o.Failure); f != "" {
		rows = append(rows, [2]string{"Failure", f})
	}
	if o.RunId != nil {
		rows = append(rows, [2]string{"Cutover run", *o.RunId})
	}
	if o.ApprovedSha != nil {
		rows = append(rows, [2]string{"Approved", shortSHA(*o.ApprovedSha) + " by " + str(o.ApprovedBy) + " at " + stampPtr(o.ApprovedAt)})
	}
	rows = append(rows, [2]string{"Created", stamp(o.CreatedAt) + " by " + o.CreatedBy}, [2]string{"Updated", stamp(o.UpdatedAt)})
	b.WriteString(fields(rows))
	if o.LegacyDeployStopped {
		b.WriteString("\n" + legacyStoppedNotice + "\n")
	}
	if s := humanActionsText(o.HumanActions); s != "" {
		b.WriteString("\nTo do by you (the platform checks it; \"charlie onboard resume " + o.Id + "\" checks now)\n" + s)
	}
	if s := factsText(o.Facts); s != "" {
		b.WriteString("\nFacts\n" + s)
	}
	b.WriteString(reportText(&o.Report))
	if s := prCheckText(o); s != "" {
		b.WriteString("\n" + s)
	}
	if o.PlannedLegacyChanges != nil && len(*o.PlannedLegacyChanges) > 0 && o.State == apiv1.OnboardingStateAwaitingApproval {
		b.WriteString("\nApproving adds to the source, in order\n" + plannedText(*o.PlannedLegacyChanges))
	}
	if len(o.LegacyChanges) > 0 {
		rows := [][]string{{"WHEN", "CHANGE", "TARGET", "UNDONE"}}
		for _, c := range o.LegacyChanges {
			rows = append(rows, []string{stamp(c.At), string(c.Kind), c.Target, stampPtr(c.UndoneAt)})
		}
		b.WriteString("\nDone on the source\n" + table(rows))
	}
	if next := onboardNext(o); next != "" {
		b.WriteString("\n" + next + "\n")
	}
	return b.String()
}

const legacyStoppedNotice = "The source's deploy workflows are disabled: pushes do not deploy the VM until the onboarding is decommissioned or deleted."

func humanActionsText(actions []apiv1.OnboardingHumanAction) string {
	var b strings.Builder
	for _, a := range actions {
		mark := "[  ]"
		if a.Done {
			mark = "[ok]"
		}
		switch a.Kind {
		case apiv1.OnboardingHumanActionKindInstallGithubApp:
			fmt.Fprintf(&b, "  %s Install the platform's GitHub App on the repository: %s\n", mark, str(a.Url))
		case apiv1.OnboardingHumanActionKindDeleteDnsRecord:
			if r := a.Record; r != nil {
				fmt.Fprintf(&b, "  %s Delete the DNS record %s %s %s, so the wildcard sends the name to the gateway\n", mark, r.Name, r.Type, r.Value)
			} else {
				fmt.Fprintf(&b, "  %s Delete the DNS record of the service address\n", mark)
			}
		default:
			fmt.Fprintf(&b, "  %s %s\n", mark, a.Kind)
		}
	}
	return b.String()
}

func factsText(f *apiv1.OnboardingFactsSummary) string {
	if f == nil {
		return ""
	}
	var rows [][2]string
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, [2]string{k, v})
		}
	}
	vm := strings.TrimSpace(fmt.Sprintf("%s %s %s", strOf(f.Cloud), str(f.Region), str(f.InstanceId)))
	if f.PublicIp != nil {
		vm += " (" + *f.PublicIp + ")"
	}
	if f.SsmManaged != nil {
		vm += ", SSM " + yesNo(*f.SsmManaged)
	}
	add("VM", vm)
	if f.DefaultBranch != nil {
		add("Default branch", *f.DefaultBranch+" at "+shortSHA(str(f.Head)))
	}
	add("Deploy workflows", joinPtr(f.DeployWorkflows))
	add("Compose project", str(f.ComposeProject))
	add("Containers", joinPtr(f.Containers))
	db := str(f.DbImage)
	if f.DbDatabase != nil {
		db += ", database " + *f.DbDatabase + " owned by " + str(f.DbOwner)
	}
	if f.DbLibc != nil {
		db += ", " + string(*f.DbLibc)
	}
	if f.DbSizeBytes != nil {
		db += ", " + bytesHuman(*f.DbSizeBytes)
	}
	add("Database", strings.TrimPrefix(db, ", "))
	add("Target image", str(f.Image))
	add("Storage", str(f.StorageSize))
	add("Reindex after", joinPtr(f.Reindex))
	if f.Bucket != nil {
		add("Files", *f.Bucket+" ("+count(f.ObjectCount)+" objects)")
	}
	if len(rows) == 0 {
		return ""
	}
	return fields(rows)
}

func reportText(r *apiv1.OnboardingReport) string {
	var b strings.Builder
	if d := r.Drift; d != nil {
		fmt.Fprintf(&b, "\nDrift: what runs differs from the default branch head %s", shortSHA(d.Head))
		if d.RunningRevision != nil {
			fmt.Fprintf(&b, "; running image built from %s", shortSHA(*d.RunningRevision))
		}
		if d.LastDeployCommit != nil {
			fmt.Fprintf(&b, "; last deploy was %s", shortSHA(*d.LastDeployCommit))
		}
		b.WriteString(". It does not block; the contract PR is made from the head.\n")
	}
	for _, s := range []struct {
		title string
		list  []apiv1.OnboardingFinding
	}{{"Blockers (it cannot go on)", r.Blockers}, {"Warnings", r.Warnings}} {
		if len(s.list) == 0 {
			continue
		}
		b.WriteString("\n" + s.title + "\n")
		for _, f := range s.list {
			subject := ""
			if f.Subject != nil {
				subject = " [" + *f.Subject + "]"
			}
			fmt.Fprintf(&b, "  %s%s: %s\n", f.Code, subject, f.Message)
		}
	}
	return b.String()
}

func prCheckText(o *apiv1.Onboarding) string {
	var b strings.Builder
	if pr := o.Pr; pr != nil {
		merged := ""
		if pr.Merged != nil && *pr.Merged {
			merged = ", merged"
		}
		fmt.Fprintf(&b, "Contract PR #%d at %s%s: %s\n", pr.Number, shortSHA(pr.HeadSha), merged, pr.Url)
		fmt.Fprintf(&b, "  Diff: %s/files\n", pr.Url)
	}
	if c := o.Check; c != nil {
		line := fmt.Sprintf("Test deployment %s: %s (attempt %d of 2)", c.App, c.Result, c.Attempts)
		if c.StartSeconds != nil {
			line += fmt.Sprintf(", started in %.1fs", *c.StartSeconds)
		}
		if bud := o.Report.HoldBudgetSeconds; bud != nil {
			line += fmt.Sprintf(", hold budget %.0fs", *bud)
			if c.StartSeconds != nil && *c.StartSeconds > *bud {
				line += " (over budget)"
			}
		}
		if h := o.Report.HoldSeconds; h != nil {
			line += fmt.Sprintf(", estimated write hold %.1fs", *h)
		}
		b.WriteString(line + "\n")
		if m := str(c.Message); m != "" {
			b.WriteString("  " + m + "\n")
		}
	}
	return b.String()
}

func plannedText(changes []apiv1.OnboardingPlannedChange) string {
	var b strings.Builder
	for i, c := range changes {
		fmt.Fprintf(&b, "  %d. %s %s\n", i+1, c.Kind, c.Target)
		if d := str(c.Detail); d != "" {
			fmt.Fprintf(&b, "     %s\n", d)
		}
	}
	return b.String()
}

// onboardNext is the command that moves the onboarding on, if a person has
// to send one.
func onboardNext(o *apiv1.Onboarding) string {
	if o.Deleting {
		if o.Failure != nil {
			return "The deletion stopped; try it again with: charlie onboard delete " + o.Id
		}
		return "Being deleted."
	}
	for _, a := range o.HumanActions {
		if !a.Done {
			return "Waiting for you; after doing it, check now with: charlie onboard resume " + o.Id
		}
	}
	switch o.State {
	case apiv1.OnboardingStateAwaitingApproval:
		return "Review the PR, then approve with: charlie onboard approve " + o.Id
	case apiv1.OnboardingStateReady:
		return "Ready to move. Cut over with: charlie onboard cutover " + o.Id
	case apiv1.OnboardingStateCuttingOver:
		if o.RunId != nil {
			return "Follow the cutover with: charlie migrate watch " + *o.RunId
		}
	case apiv1.OnboardingStateDecommissioning:
		return "After the legacy backend and the tailnet route are gone, stop the source VM with: charlie onboard decommission " + o.Id
	case apiv1.OnboardingStateDone:
		return "Done. Add environments with: charlie app env add " + o.App + " ENV"
	case apiv1.OnboardingStateFailed:
		return "Carry on from where it failed with: charlie onboard resume " + o.Id + ", or remove it with: charlie onboard delete " + o.Id
	}
	return ""
}

// followOnboarding prints an onboarding's snapshot and then its steps until
// it is done or deleted.
func (g *globals) followOnboarding(cmd *cobra.Command, c *client.Client, id string) error {
	ctx := cmd.Context()
	p := g.out()
	var last *apiv1.Onboarding
	err := c.WatchOnboarding(ctx, id, func(m client.OnboardingMessage) error {
		if g.json {
			if m.Snapshot != nil {
				last = m.Snapshot
				return p.jsonLine(jsonMessage{Event: m.Name, Data: m.Snapshot})
			}
			return p.jsonLine(jsonMessage{Event: m.Name, ID: m.Seq, Data: m.Event})
		}
		if m.Snapshot != nil {
			prev := last
			last = m.Snapshot
			if prev == nil {
				return p.text(renderOnboarding(last) + "\n")
			}
			return p.text(snapshotChanges(prev, last))
		}
		return p.text(onboardEventLine(m.Event) + "\n")
	})
	if err != nil {
		if ctx.Err() != nil {
			return &exitError{code: ExitInterrupted, msg: fmt.Sprintf(
				"interrupted; onboarding %s keeps going on the server. Reattach with: charlie onboard status %s --follow", id, id)}
		}
		return onboardError(err, id)
	}
	if g.json || last == nil {
		return nil
	}
	if last.Deleting {
		return p.text(fmt.Sprintf("Onboarding %s was deleted.\n", id))
	}
	return nil // the last snapshot printed the next step
}

// snapshotChanges is what a new snapshot says that the events do not.
func snapshotChanges(prev, cur *apiv1.Onboarding) string {
	var b strings.Builder
	if cur.State != prev.State || cur.Deleting != prev.Deleting {
		fmt.Fprintf(&b, "%s  state %s\n", clock(cur.UpdatedAt), onboardStateText(cur))
		if next := onboardNext(cur); next != "" {
			b.WriteString("          " + next + "\n")
		}
	}
	if cur.LegacyDeployStopped && !prev.LegacyDeployStopped {
		b.WriteString("          " + legacyStoppedNotice + "\n")
	}
	if !slices.EqualFunc(cur.HumanActions, prev.HumanActions, func(a, b apiv1.OnboardingHumanAction) bool {
		return a.Kind == b.Kind && a.Done == b.Done
	}) {
		b.WriteString(humanActionsText(cur.HumanActions))
	}
	if cur.RunId != nil && prev.RunId == nil {
		b.WriteString("          cutover run " + *cur.RunId + ": charlie migrate watch " + *cur.RunId + "\n")
	}
	return b.String()
}

func onboardEventLine(e *apiv1.OnboardingEvent) string {
	msg := strings.Join(strings.Fields(str(e.Message)), " ")
	switch e.Kind {
	case apiv1.OnboardingEventKindLog:
		return clock(e.Time) + "  " + string(e.State) + "  " + msg
	case apiv1.OnboardingEventKindError:
		return clock(e.Time) + "  [!!] " + string(e.State) + ": " + msg
	case apiv1.OnboardingEventKindState:
		return strings.TrimSuffix(clock(e.Time)+"  >> "+string(e.State)+": "+msg, ": ")
	default:
		return clock(e.Time) + "  " + string(e.Kind) + " " + string(e.State) + ": " + msg
	}
}

func (g *globals) onboardApproveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "approve ID",
		Short: "Approve the contract PR and what will be added to the source (role operator)",
		Long: `Reads the onboarding and prints what you approve: the blockers and
warnings, the contract PR and its head commit, the test deployment's start
time against the hold budget, and what connecting adds to the source VM and
database. Then asks you to type the app's name unless --yes is given, and
approves that PR head commit.

When the PR changed or fell behind the default branch since it was read, the
server refuses with pr_head_mismatch and makes and tests the branch again;
review it again and approve again. Once approved, the source's deploy
workflows are disabled until the onboarding is decommissioned or deleted.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			o, err := c.GetOnboarding(ctx, id)
			cancel()
			if err != nil {
				return onboardError(err, id)
			}
			if o.State != apiv1.OnboardingStateAwaitingApproval {
				return fmt.Errorf("onboarding %s is %s; it can be approved only in awaiting_approval", id, onboardStateText(o))
			}
			if o.Pr == nil {
				return fmt.Errorf("onboarding %s has no PR to approve yet", id)
			}
			// The plan goes to stderr with --json, so stdout stays the API answer.
			var w io.Writer = g.app.Stdout
			if g.json {
				w = g.app.Stderr
			}
			fmt.Fprint(w, approvalText(o))
			if !yes {
				ok, err := g.confirm("Type the app name to approve: ", o.App)
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: ExitError, msg: fmt.Sprintf("onboarding %s was not approved; type the app name to confirm, or pass --yes", id)}
				}
			}
			ctx, cancel = callCtx(cmd)
			defer cancel()
			approved, err := c.ApproveOnboarding(ctx, id, o.Pr.HeadSha)
			if err != nil {
				return onboardError(err, id)
			}
			return show(g, approved, func(a *apiv1.Onboarding) string {
				return fmt.Sprintf("Approved %s at %s. Onboarding %s is %s.\nFollow it with: charlie onboard status %s --follow\n",
					fmt.Sprintf("PR #%d", o.Pr.Number), shortSHA(o.Pr.HeadSha), id, a.State, id)
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve without asking")
	return cmd
}

func approvalText(o *apiv1.Onboarding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Approving onboarding %s of app %s (%s) to %s.\n", o.Id, o.App, o.Repo, o.TargetEnv)
	b.WriteString(reportText(&o.Report))
	b.WriteString("\n" + prCheckText(o))
	if o.PlannedLegacyChanges != nil && len(*o.PlannedLegacyChanges) > 0 {
		b.WriteString("\nThis adds to the source, in order\n" + plannedText(*o.PlannedLegacyChanges))
	}
	wf := ""
	if o.Facts != nil && o.Facts.DeployWorkflows != nil && len(*o.Facts.DeployWorkflows) > 0 {
		wf = " (" + strings.Join(*o.Facts.DeployWorkflows, ", ") + ")"
	}
	fmt.Fprintf(&b, "\nThe source's deploy workflows%s are disabled from now until the onboarding is decommissioned or deleted.\n", wf)
	return b.String()
}

func (g *globals) onboardCutoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cutover ID",
		Short: "Move the app to the platform, in ready only (role operator)",
		Long: `Starts the onboarding's own migration run: the source database is fenced,
writes are held at the gateway, the platform's replica is promoted and the
app starts there. Follow the run with "charlie migrate watch RUNID". When it
is rolled back or cancelled before the promotion, the onboarding is ready
again; when it halts after the promotion, resume the run with "charlie
migrate resume RUNID". There is no rollback after the promotion.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			o, err := c.CutOverOnboarding(ctx, id)
			if err != nil {
				return onboardError(err, id)
			}
			return show(g, o, func(o *apiv1.Onboarding) string {
				run := str(o.RunId)
				return fmt.Sprintf("Started the cutover of app %s: run %s.\nFollow it with: charlie migrate watch %s\n", o.App, run, run)
			})
		},
	}
}

func (g *globals) onboardDecommissionCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "decommission ID",
		Short: "Stop the source VM, in decommissioning only (role operator)",
		Long: `Confirms stopping the source VM after the cutover, once the legacy backend
left the gateway and the tailnet device and route were removed. The platform
stops (never terminates) the tagged instance and the onboarding is done. The
disk and the source bucket stay for you to delete in the cloud. Asks you to
type the app's name unless --yes is given.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if !yes {
				ctx, cancel := callCtx(cmd)
				o, err := c.GetOnboarding(ctx, id)
				cancel()
				if err != nil {
					return onboardError(err, id)
				}
				vm, disk := "the source VM", ""
				if f := o.Facts; f != nil {
					if f.InstanceId != nil {
						vm = "instance " + *f.InstanceId
						if f.Region != nil {
							vm += " in " + *f.Region
						}
					}
					if f.Bucket != nil {
						disk = " and the bucket " + *f.Bucket
					}
				}
				prompt := fmt.Sprintf("This stops %s, where app %s ran before. It is not terminated: its disk%s stay for you to delete in the cloud.\nType the app name to confirm: ", vm, o.App, disk)
				ok, err := g.confirm(prompt, o.App)
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: ExitError, msg: fmt.Sprintf("the source of onboarding %s was not stopped; type the app name to confirm, or pass --yes", id)}
				}
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			o, err := c.DecommissionOnboarding(ctx, id)
			if err != nil {
				return onboardError(err, id)
			}
			return show(g, o, func(o *apiv1.Onboarding) string {
				return fmt.Sprintf("Stopping the source VM of app %s. Onboarding %s is %s.\n", o.App, id, o.State)
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "stop without asking")
	return cmd
}

func (g *globals) onboardResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume ID",
		Short: "Check again and carry on, from failed or a step waiting for you (role operator)",
		Long: `From failed, goes back to the state it failed in (a cutover whose run never
started goes back to ready). While it waits for you, such as installing the
GitHub App or deleting the DNS record, checks again now instead of at the
next poll. A failed cutover run is resumed with "charlie migrate resume
RUNID" instead.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			o, err := c.ResumeOnboarding(ctx, id)
			if err != nil {
				return onboardError(err, id)
			}
			return show(g, o, func(o *apiv1.Onboarding) string {
				return fmt.Sprintf("Resumed onboarding %s: it is %s.\n", id, onboardStateText(o))
			})
		},
	}
}

func (g *globals) onboardDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete ID",
		Short: "Stop and remove an onboarding, undoing what it did (role operator)",
		Long: `Stops the hopai job and the temporary app, then by state: up to
awaiting_approval closes the PR; from connecting to ready undoes everything
done to the source in reverse order (and turns its deploy workflows back on)
and deletes the gateway app and the registered app, while a merged PR stays
merged. From cutting_over on it cannot be deleted. Answers at once; the
onboarding is gone when "charlie onboard status" answers onboarding_not_found.
When a step of the deletion failed, sending it again retries. Asks you to
type the app's name unless --yes is given.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if !yes {
				ctx, cancel := callCtx(cmd)
				o, err := c.GetOnboarding(ctx, id)
				cancel()
				if err != nil {
					return onboardError(err, id)
				}
				ok, err := g.confirm(deletePrompt(o)+"Type the app name to confirm: ", o.App)
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: ExitError, msg: fmt.Sprintf("onboarding %s was not deleted; type the app name to confirm, or pass --yes", id)}
				}
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			o, err := c.DeleteOnboarding(ctx, id)
			if err != nil {
				return onboardError(err, id)
			}
			return show(g, o, func(o *apiv1.Onboarding) string {
				return fmt.Sprintf("Deleting onboarding %s of app %s. Follow it with: charlie onboard status %s --follow\n", id, o.App, id)
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return cmd
}

// deletePrompt says what DELETE undoes in the onboarding's state.
func deletePrompt(o *apiv1.Onboarding) string {
	state := o.State
	if state == apiv1.OnboardingStateFailed && o.FailedState != nil {
		state = *o.FailedState
	}
	head := fmt.Sprintf("This stops onboarding %s of app %s: the hopai job and the test app are deleted", o.Id, o.App)
	switch state {
	case apiv1.OnboardingStateDiscovering, apiv1.OnboardingStatePatching, apiv1.OnboardingStateChecking, apiv1.OnboardingStateAwaitingApproval:
		pr := ""
		if o.Pr != nil {
			pr = fmt.Sprintf(" and PR #%d is closed", o.Pr.Number)
		}
		return head + pr + ". Nothing on the source was changed.\n"
	case apiv1.OnboardingStateConnecting, apiv1.OnboardingStateRegistering, apiv1.OnboardingStateSyncing, apiv1.OnboardingStateReady:
		var b strings.Builder
		b.WriteString(head + ", then what was done to the source is undone in reverse order:\n")
		for i := len(o.LegacyChanges) - 1; i >= 0; i-- {
			if ch := o.LegacyChanges[i]; ch.UndoneAt == nil {
				fmt.Fprintf(&b, "  - %s %s\n", ch.Kind, ch.Target)
			}
		}
		fmt.Fprintf(&b, "The gateway app and the registered app %s are deleted. A merged PR stays merged.\n", o.App)
		return b.String()
	}
	return head + ".\n"
}

// onboardError adds what to do next to the refusals that have one; the
// error itself reads "code: message".
func onboardError(err error, id string) error {
	ref := id
	if ref == "" {
		ref = "ID"
	}
	var hint string
	switch client.ErrorCode(err) {
	case "not_implemented":
		hint = "The server has the onboarding API but does not carry it out yet (ADR 0042 is being built)."
	case "pr_head_mismatch":
		hint = "The PR changed or fell behind the default branch since it was read; the platform makes and tests the branch again. Review it again with: charlie onboard status " + ref + ", then approve again."
	case "onboarding_exists":
		hint = "The app already has an onboarding that is not done; see it with: charlie onboard list, then resume or delete it."
	case "onboarding_state_conflict":
		hint = "Not in this state; see its state and the next command with: charlie onboard status " + ref
	case "onboarding_deleting":
		hint = "It is being deleted; only charlie onboard delete " + ref + " is accepted, to retry a failed deletion."
	case "app_exists":
		hint = "An app of that name is registered already; see it with: charlie app list"
	case "invalid_request":
		hint = "--repo is OWNER/NAME and --url is https://LABEL.<app domain>; LABEL is the app's name."
	case "cluster_busy":
		hint = "Another migration runs on the cluster; see it with: charlie migrate list"
	default:
		return err
	}
	return fmt.Errorf("%w\n%s", err, hint)
}

func strOf[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func joinPtr(p *[]string) string {
	if p == nil {
		return ""
	}
	return strings.Join(*p, ", ")
}
