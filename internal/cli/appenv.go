package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

func (g *globals) appEnvCmd() *cobra.Command {
	return group("env", "Add an environment to a registered app or take one back out",
		g.appEnvAddCmd(), g.appEnvRemoveCmd())
}

func (g *globals) appEnvAddCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "add APP ENV",
		Short: "Add an environment to a registered app and deploy it there (role operator)",
		Long: `Adds ENV, an environment of the server ("charlie env list"), to a registered
app and queues a deployment of the default instance (trigger environment) to
all of the app's environments, ENV included. The deployment repeats the
default instance's last healthy deployment: its commit, images and
platform.yaml, with the secrets as they are now. Nothing is built.

A postgres app gets ENV only when its database pair has ENV as the peer; the
database there becomes a streaming replica of the app's database, and
"charlie app show" says when a migration can move the app there. Branch
instances go to ENV at their next deployment. Sending it again after a
failed deployment queues a new one.

With --watch, follows the deployment and exits 0 when it is healthy and 3
otherwise.`,
		Args: exactArgs("APP", "ENV"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, env := args[0], args[1]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			added, err := c.AddAppEnvironment(ctx, app, apiv1.AppEnvironmentAddRequest{Environment: env})
			if err != nil {
				return appEnvError(err, app, env)
			}
			d := &added.Deployment
			p := g.out()
			switch {
			case g.json && watch:
				err = p.jsonLine(added)
			case g.json:
				err = p.json(added)
			default:
				msg := fmt.Sprintf("Added environment %s to app %s.\n", env, app) + queuedLine(d)
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
	cmd.Flags().BoolVar(&watch, "watch", false, "watch the deployment after adding the environment")
	return cmd
}

func (g *globals) appEnvRemoveCmd() *cobra.Command {
	var yes, watch bool
	cmd := &cobra.Command{
		Use:   "remove APP ENV",
		Short: "Take an environment back out of a registered app, deleting what runs there (role operator)",
		Long: `Takes ENV back out of a registered app, for an environment added by mistake
or whose deployment cannot get healthy. The platform removes every instance's
apps/<instance>/ENV.yaml from the gitops repository and the traffic gateway's
backend there, and Argo CD deletes the instances' namespaces in ENV: their
copy of the database and their files (buckets) there are deleted and cannot
be recovered. The database pair stays, so ENV can be added again. Then a
deployment of the default instance to the remaining environments is queued
when it has a healthy deployment to repeat.

The app's first environment and an environment that is or was the write
backend cannot be removed. Asks you to type the environment name unless
--yes is given. With --watch, follows the deployment to the remaining
environments, when one was queued.`,
		Args: exactArgs("APP", "ENV"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, env := args[0], args[1]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if !yes {
				ctx, cancel := callCtx(cmd)
				a, err := c.GetApp(ctx, app)
				cancel()
				if err != nil {
					return err
				}
				if a.Deploy == nil {
					return fmt.Errorf("app %s is only in the server's apps file, not registered from GitHub; it has no environments to remove", app)
				}
				if !slices.Contains(a.Deploy.Environments, env) {
					return fmt.Errorf("app %s is not in environment %s; its environments are %s", app, env, strings.Join(a.Deploy.Environments, ", "))
				}
				names := make([]string, len(a.Deploy.Instances))
				for i, in := range a.Deploy.Instances {
					names[i] = in.Name
				}
				if len(names) == 0 {
					names = []string{app}
				}
				prompt := fmt.Sprintf("This removes environment %s from app %s. Argo CD deletes the %s of %s in %s, with their copy of the database and their files. The data there cannot be recovered.\nType the environment name to confirm: ",
					env, app, plural(len(names), "namespace"), strings.Join(names, ", "), env)
				ok, err := g.confirm(prompt, env)
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: ExitError, msg: fmt.Sprintf("environment %s was not removed from app %s; type its name to confirm, or pass --yes", env, app)}
				}
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			removed, err := c.RemoveAppEnvironment(ctx, app, env)
			if err != nil {
				return appEnvError(err, app, env)
			}
			d := removed.Deployment
			p := g.out()
			switch {
			case g.json && watch:
				err = p.jsonLine(removed)
			case g.json:
				err = p.json(removed)
			default:
				msg := fmt.Sprintf("Removed environment %s from app %s. Argo CD is deleting its workloads and data there.\n", env, app)
				if d == nil {
					msg += "No deployment was queued: the default instance has no healthy deployment to repeat.\n"
				} else {
					msg += queuedLine(d)
					if !watch {
						msg += "Watch it with: charlie deploy watch " + d.Id + "\n"
					}
				}
				err = p.text(msg)
			}
			if err != nil || !watch || d == nil {
				return err
			}
			return g.watchDeploy(cmd, c, d.Id)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&yes, "yes", "y", false, "remove without asking")
	f.BoolVar(&watch, "watch", false, "watch the deployment to the remaining environments, when one was queued")
	return cmd
}

// appEnvError adds what to do next to the refusals of adding or removing
// an environment that have one; the error itself reads "code: message".
func appEnvError(err error, app, env string) error {
	var hint string
	switch client.ErrorCode(err) {
	case "env_not_found":
		hint = "See the server's environments with: charlie env list, and the app's with: charlie app show " + app
	case "environment_exists":
		hint = "The app already runs in " + env + " and its last deployment there is healthy."
	case "deployment_in_progress":
		hint = "Wait until that deployment ends, then try again; see it with: charlie deploy list " + app
	case "migration_in_progress":
		hint = "Wait until the app's migration ends; see it with: charlie migrate list"
	case "nothing_to_redeploy":
		hint = "Adding an environment repeats the default instance's last healthy deployment, and it has none; deploy it first: charlie deploy " + app
	case "db_pair_unavailable":
		hint = "The app's database has no pair with " + env + " as the peer, so its database there could never be migrated to; the pair is made when the app is registered."
	case "primary_environment":
		hint = "The app's first environment, where its database was born primary, stays; remove the app with: charlie app rm " + app
	case "write_backend":
		hint = env + " is or was the app's write backend; migrate the app away first."
	case "db_topology_unready":
		hint = "The two databases that would remain must be one healthy primary and a replica streaming from it; see them with: charlie app show " + app
	case "db_topology_unknown", "db_topology_unreachable":
		hint = "Nothing was removed; the server could not read or change a database Cluster of the app. Try again."
	default:
		return err
	}
	return fmt.Errorf("%w\n%s", err, hint)
}

// dbTopologyText describes the database pair of a registered app and
// whether a migration can move it now; "" when the app has no pair.
func dbTopologyText(app string, t *apiv1.AppDBTopology) string {
	if t == nil {
		return ""
	}
	present := "yes"
	if !t.ReplicaPresent {
		present = "no (add it with: charlie app env add " + app + " " + t.Replica + ")"
	}
	pairs := [][2]string{{"Primary", t.Primary}, {"Replica", t.Replica}, {"Replica in app", present}}
	if r := t.Readiness; r != nil {
		mark := "[x] no"
		if r.Ready {
			mark = "[ok] yes"
		}
		pairs = append(pairs, [2]string{"Ready to migrate", mark + " (" + r.Reason + ")"}, [2]string{"Readiness", r.Message})
	}
	out := "\nDatabase pair\n" + fields(pairs)
	if r := t.Readiness; r != nil && len(r.Environments) > 0 {
		rows := [][]string{{"ENV", "IN APP", "DEPLOYED", "GATEWAY", "ROLE", "HEALTHY", "PHASE"}}
		for _, e := range r.Environments {
			rows = append(rows, []string{e.Env, yesNo(e.InApp), yesNo(e.Deployed), yesNo(e.GatewayBackend),
				string(e.Role), yesNo(e.Healthy), str(e.Phase)})
		}
		out += table(rows)
	}
	return out
}
