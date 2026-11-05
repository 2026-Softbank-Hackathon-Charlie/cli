package cli

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// appShowDeployments is how many deployments "app show" lists.
const appShowDeployments = 5

func (g *globals) appCmd() *cobra.Command {
	return group("app", "Register apps from GitHub, see how they run and check platform.yaml",
		g.appAddCmd(), g.appListCmd(), g.appShowCmd(), g.appRmCmd(), g.appBranchCmd(), g.appSecretCmd(), g.appValidateCmd(), g.appReposCmd())
}

func (g *globals) appAddCmd() *cobra.Command {
	var (
		repo, branch  string
		envs          []string
		noAuto, watch bool
	)
	cmd := &cobra.Command{
		Use:   "add --repo OWNER/NAME --env ENV",
		Short: "Register an app from a GitHub repository and deploy it (role operator)",
		Long: `Registers the app in a GitHub repository and queues its first deployment to
every --env. The platform reads platform.yaml at the head of --branch through
its GitHub App and names the app after the file's name. Every later push to
the branch deploys again unless --no-auto-deploy is given. More branches are
deployed as instances of their own with "charlie app branch add".

An invalid platform.yaml is refused with every broken rule and where it is;
"charlie app validate" checks it first. With --watch, follows the first
deployment and exits 0 when it is healthy and 3 otherwise.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" || len(envs) == 0 {
				return usagef(cmd, "--repo and --env are required")
			}
			if !repoName(repo) {
				return usagef(cmd, "--repo must be OWNER/NAME, got %q", repo)
			}
			req := apiv1.AppCreateRequest{Repo: repo, Environments: envs}
			if cmd.Flags().Changed("branch") {
				req.Branch = &branch
			}
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
			created, err := c.CreateApp(ctx, req)
			if err != nil {
				return err
			}
			d := &created.Deployment
			p := g.out()
			switch {
			case g.json && watch:
				err = p.jsonLine(created)
			case g.json:
				err = p.json(created)
			default:
				msg := fmt.Sprintf("Registered app %s from %s.\n", created.App.Name, repo)
				if dep := created.App.Deploy; dep != nil {
					msg = fmt.Sprintf("Registered app %s from %s.\n", created.App.Name, appOrigin(dep))
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
	f.StringVar(&repo, "repo", "", "OWNER/NAME of a repository the platform's GitHub App can see (required)")
	f.StringVar(&branch, "branch", "main", "the default branch: platform.yaml is read from it and its pushes deploy the app")
	f.StringSliceVar(&envs, "env", nil, "environment to deploy to; repeat it or separate with commas (required)")
	f.BoolVar(&noAuto, "no-auto-deploy", false, "deploy only when asked (charlie deploy), not on every push")
	f.BoolVar(&watch, "watch", false, "watch the first deployment after registering")
	return cmd
}

// repoName reports whether s looks like OWNER/NAME.
func repoName(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

func appOrigin(d *apiv1.AppDeploy) string {
	return fmt.Sprintf("%s (branch %s, auto deploy %s)", d.Repo, d.Branch, onOff(d.AutoDeploy))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// queuedLine names the instance the deployment goes to; servers from before
// branch instances only name the app.
func queuedLine(d *apiv1.Deployment) string {
	what := "deployment"
	if d.Trigger == apiv1.DeploymentTriggerRedeploy {
		what = "redeploy"
	}
	return fmt.Sprintf("Queued %s %s of %s at %s (%s) to %s.\n", what, d.Id, cmp.Or(d.Instance, d.App), d.Ref, shortSHA(d.Commit), envNames(d.Environments))
}

func (g *globals) appListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered apps with their status per environment and number of instances, and the apps file's apps",
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
			return show(g, l, renderAppList)
		},
	}
}

// inAppsFile reports whether the server's apps file names the app. A
// registered app that the file does not name has no environments.
func inAppsFile(a *apiv1.App) bool { return a.Deploy == nil || len(a.Environments) > 0 }

func renderAppList(l *apiv1.AppList) string {
	reg := [][]string{{"APP", "INSTANCES", "REPO", "BRANCH", "AUTO DEPLOY", "ENV", "STATUS", "COMMIT", "URL"}}
	file := [][]string{{"APP", "NAMESPACE", "DATABASE", "ENVS"}}
	for i := range l.Apps {
		a := &l.Apps[i]
		if d := a.Deploy; d != nil {
			envs := d.Environments
			if len(envs) == 0 {
				envs = []string{""}
			}
			instances := ""
			if n := len(d.Instances); n > 0 {
				instances = fmt.Sprint(n)
			}
			for _, env := range envs {
				st := appEnvStatus(d.Status, env)
				reg = append(reg, []string{a.Name, instances, d.Repo, d.Branch, yesNo(d.AutoDeploy), env,
					envMark(st.Status) + " " + string(st.Status), shortSHA(str(st.Commit)), str(d.Url)})
			}
		}
		if inAppsFile(a) {
			names := make([]string, len(a.Environments))
			for j, e := range a.Environments {
				names[j] = e.Env
			}
			file = append(file, []string{a.Name, a.Namespace, a.Database.User + "@" + a.Database.Name, strings.Join(names, ", ")})
		}
	}
	var b strings.Builder
	if len(reg) > 1 {
		b.WriteString("Registered from GitHub\n" + table(reg))
	}
	if len(file) > 1 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("In the apps file, for migrations\n" + table(file))
	}
	if b.Len() == 0 {
		return "No apps. Register one with: charlie app add --repo OWNER/NAME --env ENV\n"
	}
	return b.String()
}

// appEnvStatus is an app's or an instance's state in env; pending when the
// server has none.
func appEnvStatus(status []apiv1.AppEnvironmentStatus, env string) apiv1.AppEnvironmentStatus {
	for _, s := range status {
		if s.Env == env {
			return s
		}
	}
	return apiv1.AppEnvironmentStatus{Env: env, Status: apiv1.DeploymentEnvironmentStatusPending}
}

// appDetail is what "app show --json" prints.
type appDetail struct {
	App         apiv1.App          `json:"app"`
	Deployments []apiv1.Deployment `json:"deployments"`
}

func (g *globals) appShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Short: "Show an app: where it comes from, its platform.yaml, its status, instances and latest deployments",
		Args:  exactArgs("NAME"),
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
			d := &appDetail{App: *a, Deployments: []apiv1.Deployment{}}
			if a.Deploy != nil {
				l, err := c.ListDeployments(ctx, a.Name, appShowDeployments)
				if err != nil {
					return err
				}
				d.Deployments = l.Deployments
			}
			return show(g, d, func(d *appDetail) string { return renderAppDetail(d, time.Now()) })
		},
	}
}

func renderAppDetail(d *appDetail, now time.Time) string {
	a := &d.App
	pairs := [][2]string{{"App", a.Name}, {"Namespace", a.Namespace}}
	dep := a.Deploy
	if dep != nil {
		pairs = append(pairs, [][2]string{
			{"Repo", dep.Repo},
			{"Branch", dep.Branch},
			{"Auto deploy", yesNo(dep.AutoDeploy)},
			{"Environments", strings.Join(dep.Environments, ", ")},
			{"URL", str(dep.Url)},
			{"Registered", stamp(dep.CreatedAt) + " by " + dep.CreatedBy},
		}...)
	}
	out := fields(pairs)
	if dep != nil {
		out += fmt.Sprintf("\nplatform.yaml at %s: %s\n%s", shortSHA(dep.SpecCommit), specLine(&dep.Spec), specTable(&dep.Spec))
		rows := [][]string{{"ENV", "STATUS", "SYNC", "HEALTH", "COMMIT", "URL", "UPDATED"}}
		for _, env := range dep.Environments {
			st := appEnvStatus(dep.Status, env)
			rows = append(rows, []string{env, envMark(st.Status) + " " + string(st.Status), str(st.SyncStatus), str(st.HealthStatus),
				shortSHA(str(st.Commit)), str(st.Url), stampPtr(st.UpdatedAt)})
		}
		out += "\n" + table(rows)
		if len(dep.Instances) > 0 {
			out += "\nInstances\n" + instanceTable(dep)
		}
		if len(d.Deployments) == 0 {
			out += "\nNo deployments yet.\n"
		} else {
			out += "\nLatest deployments\n" + renderDeployments(d.Deployments, now)
		}
	}
	if len(a.Environments) > 0 {
		rows := [][]string{{"ENV", "CLUSTER", "DB HOST", "BUCKET"}}
		for _, e := range a.Environments {
			rows = append(rows, []string{e.Env, e.CnpgCluster, e.DbHost, str(e.Bucket)})
		}
		out += fmt.Sprintf("\nIn the apps file, for migrations (database %s@%s)\n%s", a.Database.User, a.Database.Name, table(rows))
	}
	if dep == nil {
		out += "\nNot registered from GitHub. Register its repository with: charlie app add --repo OWNER/NAME --env ENV\n"
	}
	return out
}

// specLine sums up a platform.yaml in one line.
func specLine(s *apiv1.AppSpec) string {
	line := fmt.Sprintf("app %s, %d %s", s.Name, len(s.Services), plural(len(s.Services), "service"))
	var res []string
	if s.Resources != nil {
		for _, r := range *s.Resources {
			res = append(res, r.Name+" ("+r.Type+")")
		}
	}
	if len(res) == 0 {
		return line + ", no resources"
	}
	return line + ", resources " + strings.Join(res, ", ")
}

func specTable(s *apiv1.AppSpec) string {
	rows := [][]string{{"SERVICE", "PORT", "PUBLIC", "REPLICAS", "DOCKERFILE", "HEALTH CHECK", "ENV"}}
	for _, sv := range s.Services {
		var env []string
		if sv.Env != nil {
			for _, k := range slices.Sorted(maps.Keys(*sv.Env)) {
				env = append(env, k+"="+(*sv.Env)[k])
			}
		}
		port := "-" // a worker listens on no port (platform ADR 0024)
		if sv.Port != 0 {
			port = fmt.Sprint(sv.Port)
		}
		rows = append(rows, []string{sv.Name, port, yesNo(sv.Public), fmt.Sprint(sv.Replicas),
			path.Join(sv.Path, sv.Dockerfile), str(sv.HealthCheck), clip(strings.Join(env, ", "), 60)})
	}
	return table(rows)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func (g *globals) appRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm NAME",
		Short: "Unregister an app and delete everything it runs, data included (role operator)",
		Long: `Unregisters an app registered from GitHub. The platform removes apps/NAME/ and
the apps/INSTANCE/ of every instance added with "charlie app branch add" from
the gitops repository, and Argo CD deletes the workloads of every instance in
every environment together with their databases and buckets: the data cannot
be recovered. Asks you to type the app's name unless --yes is given. An app
that is only in the server's apps file cannot be removed.`,
		Args: exactArgs("NAME"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if !yes {
				ctx, cancel := callCtx(cmd)
				a, err := c.GetApp(ctx, name)
				cancel()
				if err != nil {
					return err
				}
				if a.Deploy == nil {
					return fmt.Errorf("app %s is only in the server's apps file, not registered from GitHub; there is nothing to remove", name)
				}
				var added []string
				for _, in := range a.Deploy.Instances {
					if !in.Default {
						added = append(added, in.Name)
					}
				}
				also := ""
				if len(added) > 0 {
					also = fmt.Sprintf(", together with its added %s %s", plural(len(added), "instance"), strings.Join(added, ", "))
				}
				prompt := fmt.Sprintf("This deletes app %s from %s with its databases and buckets%s. The data cannot be recovered.\nType the app name to confirm: ",
					name, strings.Join(a.Deploy.Environments, ", "), also)
				ok, err := g.confirm(prompt, name)
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: ExitError, msg: fmt.Sprintf("app %s was not deleted; type its name to confirm, or pass --yes", name)}
				}
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			if err := c.DeleteApp(ctx, name); err != nil {
				return err
			}
			if g.json { // the API answers 202 without a body
				return nil
			}
			return g.out().text(fmt.Sprintf("Deleted app %s. Argo CD is removing its workloads and data.\n", name))
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return cmd
}

// confirm prints prompt to stderr and reports whether the next line of stdin is want.
func (g *globals) confirm(prompt, want string) (bool, error) {
	fmt.Fprint(g.app.Stderr, prompt)
	line, err := bufio.NewReader(g.app.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read the answer: %w", err)
	}
	if f, ok := g.app.Stdin.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		fmt.Fprintln(g.app.Stderr) // nobody typed the newline
	}
	return strings.TrimSpace(line) == want, nil
}

func (g *globals) appValidateCmd() *cobra.Command {
	var (
		file, repo, branch string
		remote             bool
	)
	cmd := &cobra.Command{
		Use:   "validate [-f platform.yaml]",
		Short: "Check a platform.yaml locally, or a repository's through the platform",
		Long: `Checks platform.yaml with the rules the platform applies when an app is
registered and before every deployment, and prints every broken rule with where
it is, such as services[0].port. Exits 0 when the file is valid and 1 when it
is not. -f - reads stdin. The local check needs no login.

With --remote --repo OWNER/NAME the platform reads platform.yaml at the head of
--branch through its GitHub App instead, and also checks that every service's
Dockerfile is in the repository.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fl := cmd.Flags()
			if !remote {
				for _, n := range []string{"repo", "branch"} {
					if fl.Changed(n) {
						return usagef(cmd, "--%s needs --remote", n)
					}
				}
				data, err := g.readFile(file)
				if err != nil {
					return err
				}
				what := file
				if file == "-" {
					what = appspec.FileName + " from stdin"
				}
				return g.showValidation(localValidation(data), what)
			}
			if repo == "" {
				return usagef(cmd, "--remote needs --repo OWNER/NAME")
			}
			if fl.Changed("file") {
				return usagef(cmd, "-f checks a local file; leave it out with --remote")
			}
			if !repoName(repo) {
				return usagef(cmd, "--repo must be OWNER/NAME, got %q", repo)
			}
			req := apiv1.AppSpecValidateRequest{Repo: &repo}
			if fl.Changed("branch") {
				req.Branch = &branch
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			v, err := c.ValidateAppSpec(ctx, req)
			if err != nil {
				return err
			}
			return g.showValidation(v, fmt.Sprintf("%s in %s (%s)", appspec.FileName, repo, branch))
		},
	}
	f := cmd.Flags()
	f.StringVarP(&file, "file", "f", appspec.FileName, "the file to check, or - for stdin")
	f.BoolVar(&remote, "remote", false, "have the platform check platform.yaml in a GitHub repository (needs --repo)")
	f.StringVar(&repo, "repo", "", "with --remote: OWNER/NAME of the repository")
	f.StringVar(&branch, "branch", "main", "with --remote: the branch to read")
	return cmd
}

// localValidation checks platform.yaml with pkg/appspec, the server's rules,
// and answers as POST /v1/apps/validate would.
func localValidation(data []byte) *apiv1.AppSpecValidation {
	spec, errs := appspec.Check(data)
	v := &apiv1.AppSpecValidation{Valid: len(errs) == 0, Errors: []apiv1.AppSpecFieldError{}}
	for _, e := range errs {
		v.Errors = append(v.Errors, apiv1.AppSpecFieldError{Path: e.Path, Message: e.Message})
	}
	if spec != nil {
		var s apiv1.AppSpec
		if decodeAs(spec, &s) { // appspec.Spec's JSON is the API's AppSpec
			v.Spec = &s
		}
	}
	return v
}

// showValidation prints a check's result; an invalid file exits 1.
func (g *globals) showValidation(v *apiv1.AppSpecValidation, what string) error {
	if err := show(g, v, func(v *apiv1.AppSpecValidation) string { return renderValidation(v, what) }); err != nil {
		return err
	}
	if !v.Valid {
		return &exitError{code: ExitError}
	}
	return nil
}

func renderValidation(v *apiv1.AppSpecValidation, what string) string {
	if c := str(v.Commit); c != "" {
		what += " at " + shortSHA(c)
	}
	if !v.Valid {
		return fmt.Sprintf("%s is invalid (%d %s):\n%s", what, len(v.Errors), plural(len(v.Errors), "error"), specErrors(v.Errors))
	}
	if v.Spec == nil {
		return what + " is valid.\n"
	}
	return fmt.Sprintf("%s is valid: %s.\n\n%s", what, specLine(v.Spec), specTable(v.Spec))
}

// specErrors lists the rules a platform.yaml breaks, one per line with its
// path; a problem with the file as a whole has none.
func specErrors(errs []apiv1.AppSpecFieldError) string {
	var b strings.Builder
	for _, e := range errs {
		if e.Path == "" {
			fmt.Fprintf(&b, "  %s\n", e.Message)
		} else {
			fmt.Fprintf(&b, "  %s: %s\n", e.Path, e.Message)
		}
	}
	return b.String()
}

func (g *globals) appReposCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repos",
		Short: "List the GitHub repositories the platform's GitHub App can see",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListGitHubRepos(ctx)
			if err != nil {
				return err
			}
			return show(g, l, renderRepos)
		},
	}
}

func renderRepos(l *apiv1.GitHubRepoList) string {
	if len(l.Repos) == 0 {
		return "The platform's GitHub App sees no repositories; install it on the repository first.\n"
	}
	rows := [][]string{{"REPO", "VISIBILITY", "DEFAULT BRANCH", "URL"}}
	for _, r := range l.Repos {
		vis := "public"
		if r.Private {
			vis = "private"
		}
		rows = append(rows, []string{r.FullName, vis, r.DefaultBranch, r.HtmlUrl})
	}
	return table(rows)
}

func (g *globals) deployCmd() *cobra.Command {
	var (
		branch, ref     string
		envs            []string
		watch, redeploy bool
	)
	cmd := &cobra.Command{
		Use:   "deploy APP",
		Short: "Deploy a registered app now; list, show and follow its deployments",
		Long: `Deploys a registered app now, to all its environments or to the --env ones
(role operator). --branch picks the instance: the app's own branch (the
default) or a branch added with "charlie app branch add". --ref is the branch,
tag or commit sha to deploy into that instance; by default the head of the
branch. The platform builds one image per service, commits
apps/<instance>/<env>.yaml to the gitops repository and Argo CD syncs it.

--redeploy builds nothing: it deploys the instance's last healthy deployment
again, the same commit and images, with the app's secrets as they are now.
Use it after "charlie app secret set" or "rm". It cannot be combined with
--ref, and it fails when the instance has no healthy deployment yet or a
deployment of it is still going.

With --watch, follows the deployment and exits 0 when it is healthy and 3 when
it failed or a newer deployment of the instance superseded it.`,
		Args: exactArgs("APP"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var req apiv1.DeploymentCreateRequest
			if cmd.Flags().Changed("branch") { // the server refuses an empty one
				req.Branch = &branch
			}
			if redeploy {
				if cmd.Flags().Changed("ref") {
					return usagef(cmd, "--ref cannot be used with --redeploy, which deploys the commit of the instance's last healthy deployment")
				}
				req.Redeploy = &redeploy
			}
			if ref != "" {
				req.Ref = &ref
			}
			if len(envs) > 0 {
				req.Environments = &envs
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if req.Branch == nil && ref != "" {
				g.noteRefInstance(cmd, c, args[0], ref)
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			d, err := c.CreateDeployment(ctx, args[0], req)
			if err != nil && redeploy {
				return redeployError(err, args[0], req.Branch, envs)
			}
			if err != nil {
				return err
			}
			if redeploy && d.Trigger != apiv1.DeploymentTriggerRedeploy {
				fmt.Fprintf(g.app.Stderr, "warning: the server queued a %s deployment, not a redeploy; it does not know --redeploy, so this deployment builds the head of the branch\n", d.Trigger)
			}
			p := g.out()
			switch {
			case g.json && watch:
				err = p.jsonLine(d)
			case g.json:
				err = p.json(d)
			default:
				msg := queuedLine(d)
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
	f.StringVar(&branch, "branch", "", "the instance to deploy, named by the branch it deploys (default: the app's branch)")
	f.StringVar(&ref, "ref", "", "branch, tag or commit sha to deploy into that instance (default: the head of the branch)")
	f.StringSliceVar(&envs, "env", nil, "deploy only to these of the app's environments; repeat it or separate with commas")
	f.BoolVar(&watch, "watch", false, "watch the deployment after starting it")
	f.BoolVar(&redeploy, "redeploy", false, "deploy the instance's last healthy deployment again without a build, with the current secrets")
	cmd.AddCommand(g.deployListCmd(), g.deployShowCmd(), g.deployWatchCmd(), g.deployLogsCmd())
	return cmd
}

// redeployError says what to do when a redeploy is refused. branch and envs
// are the --branch and --env given, if any.
func redeployError(err error, app string, branch *string, envs []string) error {
	flag := ""
	if branch != nil {
		flag = " --branch " + *branch
	}
	envFlag := ""
	if len(envs) > 0 {
		envFlag = " --env " + strings.Join(envs, ",")
	}
	switch client.ErrorCode(err) {
	case "nothing_to_redeploy":
		return fmt.Errorf("%w\nA redeploy repeats the instance's last healthy deployment, and it has none; deploy it with a build: charlie deploy %s%s%s", err, app, flag, envFlag)
	case "deployment_in_progress":
		return fmt.Errorf("%w\nWait until that deployment ends, then redeploy; see it with: charlie deploy list %s%s", err, app, flag)
	}
	return err
}

// noteRefInstance warns on stderr when ref is the branch of an added
// instance: without --branch it is deployed into the default instance. It
// says nothing when the app cannot be read.
func (g *globals) noteRefInstance(cmd *cobra.Command, c *client.Client, app, ref string) {
	ctx, cancel := callCtx(cmd)
	defer cancel()
	a, err := c.GetApp(ctx, app)
	if err != nil || a.Deploy == nil {
		return
	}
	for _, in := range a.Deploy.Instances {
		if !in.Default && in.Branch == ref {
			fmt.Fprintf(g.app.Stderr, "note: %s has its own instance %s; this deploys %s's head into the default instance %s. Use --branch %s to deploy that instance.\n",
				ref, in.Name, ref, a.Name, ref)
		}
	}
}

func (g *globals) deployListCmd() *cobra.Command {
	var (
		limit  int
		branch string
	)
	cmd := &cobra.Command{
		Use:   "list APP",
		Short: "List an app's deployments to all its instances, or to one branch's, newest first",
		Args:  exactArgs("APP"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("limit") && (limit < 1 || limit > 200) {
				return usagef(cmd, "--limit must be between 1 and 200")
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListBranchDeployments(ctx, args[0], branch, limit)
			if err != nil {
				return err
			}
			return show(g, l, func(l *apiv1.DeploymentList) string {
				if len(l.Deployments) == 0 && branch != "" {
					return "No deployments of " + args[0] + " branch " + branch + ".\n"
				}
				if len(l.Deployments) == 0 {
					return "No deployments of " + args[0] + ".\n"
				}
				return renderDeployments(l.Deployments, time.Now())
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "deployments to show, 1 to 200 (server default 20)")
	cmd.Flags().StringVar(&branch, "branch", "", "only the deployments of this branch's instance")
	return cmd
}

func (g *globals) deployShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
		Short: "Show a deployment: its status per environment and the images it built",
		Args:  exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			d, err := c.GetDeployment(ctx, args[0])
			if err != nil {
				return err
			}
			return show(g, d, func(d *apiv1.Deployment) string { return renderDeployment(d, time.Now()) })
		},
	}
}

func (g *globals) deployWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch ID",
		Short: "Follow a deployment live until it ends",
		Long: `Follows a deployment until it ends: its phase, each environment, the build and
deploy log and the elapsed time, then the instance's URL. A dropped connection
is resumed without losing or repeating events. On a terminal the view redraws
in place; otherwise it prints one line per event. Exits 0 when the deployment
is healthy and 3 when it failed or was superseded.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			return g.watchDeploy(cmd, c, args[0])
		},
	}
}

func (g *globals) deployLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs ID",
		Short: "Print a deployment's build and deploy log",
		Long: `Prints the log lines a deployment recorded, oldest first. For a deployment that
is still going it prints the lines so far; --follow keeps printing until the
deployment ends and then exits 0 when it is healthy and 3 otherwise.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if follow {
				return g.followLogs(cmd, c, args[0])
			}
			return g.storedLogs(cmd, c, args[0])
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing until the deployment ends")
	return cmd
}

func renderDeployments(ds []apiv1.Deployment, now time.Time) string {
	rows := [][]string{{"DEPLOYMENT", "STATUS", "BRANCH", "REF", "COMMIT", "TRIGGER", "BY", "ENVS", "CREATED", "DURATION"}}
	for i := range ds {
		d := &ds[i]
		envs := make([]string, len(d.Environments))
		for j, e := range d.Environments {
			envs[j] = envMark(e.Status) + " " + e.Env
		}
		rows = append(rows, []string{d.Id, deployMark(d.Status) + " " + string(d.Status), d.Branch, d.Ref, shortSHA(d.Commit),
			string(d.Trigger), d.TriggeredBy, strings.Join(envs, ", "), stamp(d.CreatedAt), human(deployElapsed(d, now))})
	}
	return table(rows)
}

func renderDeployment(d *apiv1.Deployment, now time.Time) string {
	out := fields([][2]string{
		{"Deployment", d.Id},
		{"App", d.App},
		{"Branch", d.Branch},
		{"Instance", d.Instance},
		{"Status", deployMark(d.Status) + " " + string(d.Status)},
		{"Ref", d.Ref},
		{"Commit", d.Commit},
		{"Trigger", string(d.Trigger) + " by " + d.TriggeredBy},
		{"Created", stamp(d.CreatedAt)},
		{"Started", stampPtr(d.StartedAt)},
		{"Finished", stampPtr(d.FinishedAt)},
		{"Duration", human(deployElapsed(d, now))},
		{"Build", str(d.BuildId)},
		{"Error", str(d.Error)},
	})
	out += "\n" + deployEnvTable(d.Environments)
	if len(d.Images) > 0 {
		rows := [][]string{{"SERVICE", "IMAGE"}}
		for _, im := range d.Images {
			rows = append(rows, []string{im.Service, im.Image})
		}
		out += "\n" + table(rows)
	}
	if !deployFinished(d.Status) {
		out += "\nFollow it with: charlie deploy watch " + d.Id + "\n"
	}
	return out
}

func deployEnvTable(envs []apiv1.DeploymentEnvironment) string {
	rows := [][]string{{"ENV", "STATUS", "SYNC", "HEALTH", "URL", "MESSAGE"}}
	for _, e := range envs {
		rows = append(rows, []string{e.Env, envMark(e.Status) + " " + string(e.Status), str(e.SyncStatus), str(e.HealthStatus),
			str(e.Url), clip(str(e.Message), 80)})
	}
	return table(rows)
}

func envNames(envs []apiv1.DeploymentEnvironment) string {
	names := make([]string, len(envs))
	for i, e := range envs {
		names[i] = e.Env
	}
	return strings.Join(names, ", ")
}

// deployElapsed is how long a deployment took, or has taken so far, from
// the moment it was queued.
func deployElapsed(d *apiv1.Deployment, now time.Time) time.Duration {
	if d.FinishedAt != nil {
		return d.FinishedAt.Sub(d.CreatedAt)
	}
	return now.Sub(d.CreatedAt)
}

func deployFinished(s apiv1.DeploymentStatus) bool {
	return s == apiv1.DeploymentStatusHealthy || s == apiv1.DeploymentStatusFailed || s == apiv1.DeploymentStatusSuperseded
}

func deployMark(s apiv1.DeploymentStatus) string {
	switch s {
	case apiv1.DeploymentStatusHealthy:
		return "[ok]"
	case apiv1.DeploymentStatusFailed, apiv1.DeploymentStatusSuperseded:
		return "[x]"
	case apiv1.DeploymentStatusQueued:
		return "[ ]"
	}
	return "[..]"
}

func envMark(s apiv1.DeploymentEnvironmentStatus) string {
	switch s {
	case apiv1.DeploymentEnvironmentStatusHealthy:
		return "[ok]"
	case apiv1.DeploymentEnvironmentStatusDegraded, apiv1.DeploymentEnvironmentStatusFailed:
		return "[x]"
	case apiv1.DeploymentEnvironmentStatusSyncing:
		return "[..]"
	}
	return "[ ]"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
