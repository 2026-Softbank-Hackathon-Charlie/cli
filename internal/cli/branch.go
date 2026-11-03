package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/spf13/cobra"
)

func (g *globals) appBranchCmd() *cobra.Command {
	return group("branch", "Deploy more branches of an app, each as an instance of its own",
		g.appBranchAddCmd(), g.appBranchListCmd(), g.appBranchRmCmd())
}

func (g *globals) appBranchAddCmd() *cobra.Command {
	var noAuto, watch bool
	cmd := &cobra.Command{
		Use:   "add APP BRANCH",
		Short: "Deploy another branch of an app as an instance of its own (role operator)",
		Long: `Deploys BRANCH of a registered app's repository as an instance of its own,
named APP--<branch slug>, with its own namespace, databases, buckets and URL
https://APP--<branch slug>.<domain>, and queues its first deployment to every
environment of the app. The platform reads platform.yaml at the head of BRANCH
and checks it as at registration; its name must be the app's. Every later push
to BRANCH deploys the instance again unless --no-auto-deploy is given. An app
has at most 3 instances, the default one (the app's own branch) included.

With --watch, follows the first deployment and exits 0 when it is healthy and
3 otherwise.`,
		Args: exactArgs("APP", "BRANCH"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			req := apiv1.AppInstanceCreateRequest{Branch: args[1]}
			if noAuto {
				off := false
				req.AutoDeploy = &off
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			created, err := c.CreateAppInstance(ctx, app, req)
			if err != nil {
				return err
			}
			in, d := &created.Instance, &created.Deployment
			p := g.out()
			switch {
			case g.json && watch:
				err = p.jsonLine(created)
			case g.json:
				err = p.json(created)
			default:
				msg := fmt.Sprintf("Added instance %s of app %s (branch %s, auto deploy %s).\n", in.Name, app, in.Branch, onOff(in.AutoDeploy))
				if u := str(in.Url); u != "" {
					msg += "URL: " + u + "\n"
				}
				msg += queuedLine(d)
				if !watch {
					msg += "Watch it with: charlie deploy watch " + d.Id + "\n"
				}
				err = p.text(msg)
			}
			if err != nil || !watch {
				return err
			}
			return g.watchDeploy(cmd, c, d.Id)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&noAuto, "no-auto-deploy", false, "deploy the instance only when asked (charlie deploy --branch), not on every push")
	f.BoolVar(&watch, "watch", false, "watch the first deployment after adding the branch")
	return cmd
}

func (g *globals) appBranchListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list APP",
		Short: "List an app's instances: the default one and the branches added to it",
		Args:  exactArgs("APP"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			a, err := c.GetApp(ctx, args[0])
			if err != nil {
				return err
			}
			if a.Deploy == nil {
				return notRegistered(a.Name)
			}
			ins := a.Deploy.Instances
			if ins == nil {
				ins = []apiv1.AppInstance{}
			}
			return show(g, &ins, func(*[]apiv1.AppInstance) string {
				if len(ins) == 0 {
					return "No instances of " + a.Name + ".\n"
				}
				return instanceTable(a.Deploy)
			})
		},
	}
}

func (g *globals) appBranchRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm APP INSTANCE|BRANCH",
		Short: "Remove an added instance and delete everything it runs, data included (role operator)",
		Long: `Removes an instance added with "charlie app branch add", named by the instance
or by its branch. The platform stops deploying the branch, forgets its
deployments and removes apps/INSTANCE/ from the gitops repository, and Argo CD
deletes the instance's workloads in every environment together with its
databases and buckets: the data cannot be recovered. Asks you to type the
instance name unless --yes is given. The default instance, which deploys the
app's own branch, goes only with the app ("charlie app rm").`,
		Args: exactArgs("APP", "INSTANCE|BRANCH"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			a, err := c.GetApp(ctx, name)
			cancel()
			if err != nil {
				return err
			}
			if a.Deploy == nil {
				return notRegistered(name)
			}
			in := findInstance(a.Deploy.Instances, args[1])
			if in == nil {
				return fmt.Errorf("app %s has no instance or branch %q; list them with: charlie app branch list %s", name, args[1], name)
			}
			if in.Default {
				return fmt.Errorf(`instance %s deploys the app's own branch %s; the default instance goes only with the app; use "charlie app rm %s"`, in.Name, in.Branch, name)
			}
			if !yes {
				prompt := fmt.Sprintf("This deletes instance %s (branch %s) of app %s from %s with its databases and buckets. The data cannot be recovered.\nType the instance name to confirm: ",
					in.Name, in.Branch, name, strings.Join(a.Deploy.Environments, ", "))
				ok, err := g.confirm(prompt, in.Name)
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: ExitError, msg: fmt.Sprintf("instance %s was not deleted; type its name to confirm, or pass --yes", in.Name)}
				}
			}
			ctx, cancel = callCtx(cmd)
			defer cancel()
			if err := c.DeleteAppInstance(ctx, name, in.Name); err != nil {
				return err
			}
			if g.json { // the API answers 202 without a body
				return nil
			}
			return g.out().text(fmt.Sprintf("Deleted instance %s of app %s. Argo CD is removing its workloads and data.\n", in.Name, name))
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return cmd
}

func notRegistered(app string) error {
	return fmt.Errorf("app %s is only in the server's apps file, not registered from GitHub; it has no instances", app)
}

// findInstance finds an app's instance by its name, else by its branch.
func findInstance(ins []apiv1.AppInstance, arg string) *apiv1.AppInstance {
	for i := range ins {
		if ins[i].Name == arg {
			return &ins[i]
		}
	}
	for i := range ins {
		if ins[i].Branch == arg {
			return &ins[i]
		}
	}
	return nil
}

// instanceTable lists an app's instances, one row each. STATUS marks every
// environment of the app; COMMIT lists the commits they run.
func instanceTable(d *apiv1.AppDeploy) string {
	rows := [][]string{{"INSTANCE", "BRANCH", "DEFAULT", "AUTO DEPLOY", "STATUS", "COMMIT", "URL"}}
	for i := range d.Instances {
		in := &d.Instances[i]
		var envs, commits []string
		for _, env := range d.Environments {
			st := appEnvStatus(in.Status, env)
			envs = append(envs, envMark(st.Status)+" "+env)
			if c := shortSHA(str(st.Commit)); c != "" && !slices.Contains(commits, c) {
				commits = append(commits, c)
			}
		}
		rows = append(rows, []string{in.Name, in.Branch, yesNo(in.Default), yesNo(in.AutoDeploy),
			strings.Join(envs, ", "), strings.Join(commits, ", "), str(in.Url)})
	}
	return table(rows)
}
