package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/spf13/cobra"
)

// addPlanTools adds the commands for plan drafts, attachments, apps,
// written-records streams and rollback to the command tree.
func (g *globals) addPlanTools(root *cobra.Command) {
	for _, c := range root.Commands() {
		switch c.Name() {
		case "plan":
			c.AddCommand(g.planNewCmd(), g.planAttachCmd())
		case "migrate":
			c.AddCommand(g.migrateRollbackCmd())
		}
	}
	root.AddCommand(g.appsCmd(), g.writtenRecordsCmd())
}

func (g *globals) planNewCmd() *cobra.Command {
	var (
		app, from, to, stream, out string
		noFiles, submit            bool
	)
	cmd := &cobra.Command{
		Use:   "new --app APP --from ENV --to ENV",
		Short: "Draft a plan from the server's app configuration (role agent)",
		Long: `Asks the server for a plan that moves APP from one environment to another,
built from its apps file (see "charlie apps"). Without --submit the draft is
printed (or written to -o FILE) so it can be edited and submitted with
"charlie plan submit". With --submit it is submitted as is.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app == "" || from == "" || to == "" {
				return usagef(cmd, "--app, --from and --to are required")
			}
			req := apiv1.PlanDraftRequest{App: app, Source: from, Target: to}
			if noFiles {
				f := false
				req.Files = &f
			}
			if stream != "" {
				req.WrittenRecordsStream = &stream
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			draft, err := c.DraftPlan(ctx, req)
			if err != nil {
				return err
			}
			raw, err := json.MarshalIndent(draft, "", "  ")
			if err != nil {
				return err
			}
			raw = append(raw, '\n')
			if out != "" {
				if err := os.WriteFile(out, raw, 0o644); err != nil {
					return err
				}
			}
			p := g.out()
			switch {
			case submit:
				rec, err := c.SubmitPlan(ctx, raw)
				if err != nil {
					return err
				}
				return show(g, rec, func(p *apiv1.PlanRecord) string {
					return fmt.Sprintf("Submitted plan %s.\n\n%s\nApprove it with: charlie plan approve %s\n", p.Ref, renderPlan(p), p.Ref)
				})
			case out != "" && !g.json:
				return p.text(fmt.Sprintf("Wrote plan %s to %s.\nSubmit it with: charlie plan submit -f %s\n", draft.Id, out, out))
			}
			_, err = g.app.Stdout.Write(raw)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&app, "app", "", "app name from the server's apps file (required)")
	f.StringVar(&from, "from", "", "source environment (required)")
	f.StringVar(&to, "to", "", "target environment (required)")
	f.StringVar(&stream, "stream", "", `verify the writes recorded in this written-records stream ("stream:NAME")`)
	f.BoolVar(&noFiles, "no-files", false, "leave object storage out even when both environments have a bucket")
	f.BoolVar(&submit, "submit", false, "submit the draft for approval and print the plan")
	f.StringVarP(&out, "output", "o", "", "also write the draft JSON to FILE")
	return cmd
}

func (g *globals) planAttachCmd() *cobra.Command {
	var name, file string
	cmd := &cobra.Command{
		Use:   "attach REF --name NAME -f FILE",
		Short: "Upload a file a pending plan refers to as attachment:NAME (role agent)",
		Long: `Uploads a file the plan refers to, such as the target CNPG Cluster manifest
(target.manifest: "attachment:NAME"). Only a plan in pending_approval accepts
attachments, so the approver approves the content that will run. Uploading the
same name again replaces it. -f - reads stdin.`,
		Args: exactArgs("REF"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || file == "" {
				return usagef(cmd, "--name and -f FILE are required (use -f - for stdin)")
			}
			content, err := g.readFile(file)
			if err != nil {
				return err
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			a, err := c.PutPlanAttachment(ctx, args[0], name, content)
			if err != nil {
				return err
			}
			return show(g, a, func(a *apiv1.PlanAttachment) string {
				return fmt.Sprintf("Attached %s to plan %s (%s, sha256 %s).\nThe plan refers to it as \"attachment:%s\".\n",
					a.Name, args[0], bytesHuman(a.Size), a.Sha256, a.Name)
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "attachment name, e.g. target.yaml (required)")
	cmd.Flags().StringVarP(&file, "file", "f", "", "file to upload, or - for stdin (required)")
	return cmd
}

func (g *globals) appsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apps",
		Short: "List the apps the server can draft plans for, and their environments",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListApps(ctx)
			if err != nil {
				return err
			}
			return show(g, l, renderApps)
		},
	}
}

func renderApps(l *apiv1.AppList) string {
	rows := [][]string{{"APP", "NAMESPACE", "DATABASE", "SCHEMAS", "ENV", "CLUSTER", "DB HOST", "BUCKET"}}
	for _, a := range l.Apps {
		db := a.Database.User + "@" + a.Database.Name
		for _, e := range a.Environments {
			rows = append(rows, []string{a.Name, a.Namespace, db, strings.Join(a.Database.Schemas, ","), e.Env, e.CnpgCluster, e.DbHost, str(e.Bucket)})
		}
	}
	// An app registered from GitHub only has no environments here.
	if len(rows) == 1 {
		return "No apps configured on the server (PLATFORM_APPS_FILE).\n"
	}
	return table(rows)
}

func (g *globals) writtenRecordsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "written-records STREAM",
		Short: "Show how many writes a written-records stream holds",
		Long: `Shows a written-records stream: the writes a load generator saw succeed.
A plan with verify.writtenRecords "stream:STREAM" checks every one of them on
the target database.`,
		Args: exactArgs("STREAM"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			st, err := c.GetWrittenRecordStats(ctx, args[0])
			if err != nil {
				return err
			}
			return show(g, st, func(s *apiv1.WrittenRecordStats) string {
				return fields([][2]string{{"Stream", s.Stream}, {"Records", fmt.Sprint(s.Count)}, {"Latest", stampPtr(s.LatestAt)}})
			})
		},
	}
}

func (g *globals) migrateRollbackCmd() *cobra.Command {
	var (
		reason            string
		autoSwitch, watch bool
	)
	cmd := &cobra.Command{
		Use:   "rollback RUNID",
		Short: "Migrate a succeeded run back to where it came from (role operator)",
		Long: `Builds the reverse of a succeeded run's plan (source and target swapped),
stores it approved by you and starts it. It cuts over as soon as data is in
sync unless --auto-switch=false, which waits for "charlie migrate switch".
With --watch, exits 0 when the rollback succeeded and 3 otherwise.`,
		Args: exactArgs("RUNID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var req apiv1.RollbackRequest
			if reason != "" {
				req.Reason = &reason
			}
			if cmd.Flags().Changed("auto-switch") {
				req.AutoSwitch = &autoSwitch
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			resp, err := c.RollbackMigration(ctx, args[0], req)
			if err != nil {
				return err
			}
			p := g.out()
			switch {
			case g.json && watch:
				err = p.jsonLine(resp)
			case g.json:
				err = p.json(resp)
			default:
				msg := fmt.Sprintf("Started rollback run %s of run %s (plan %s).\n", resp.RunId, args[0], resp.PlanRef)
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
	f.StringVar(&reason, "reason", "", "why the run is rolled back (kept in the plan review and the audit log)")
	f.BoolVar(&autoSwitch, "auto-switch", true, "cut over as soon as data is in sync; false waits for switch")
	f.BoolVar(&watch, "watch", false, "watch the rollback run after starting it")
	return cmd
}
