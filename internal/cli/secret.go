package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

// Limits of app secrets, as the server checks them (ADR 0019).
const (
	maxSecretName  = 64
	maxSecretValue = 4096    // bytes; a value is at least 1 byte
	maxSecretNames = 50      // names in one set and in one delete
	maxEnvFile     = 1 << 20 // bytes of an --env-file read
)

var secretNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

const instanceUsage = "an instance, by its name (APP, APP--dev) or its branch; without it the app-wide secrets"

// noValueArg says why set takes no value on the command line.
const noValueArg = "the value is never taken from the command line, where it would stay in your shell history; type it when asked, or use --stdin, --from-file or --env-file"

func (g *globals) appSecretCmd() *cobra.Command {
	return group("secret", "Keep an app's secret environment variables, app-wide or per instance",
		g.appSecretListCmd(), g.appSecretGetCmd(), g.appSecretSetCmd(), g.appSecretRmCmd())
}

func (g *globals) appSecretListCmd() *cobra.Command {
	var instance string
	cmd := &cobra.Command{
		Use:   "list APP",
		Short: "List an app's secrets by name, never their values",
		Long: `Lists the names of an app's secrets with when and by whom each was last set,
never their values: the app-wide ones, which every instance gets, and each
instance's overrides. With --instance, lists the secrets that instance gets
instead, with where each comes from: app (the app-wide value), override (the
instance's own value over an app-wide one) or instance-only (set only for the
instance). Role viewer.`,
		Args: exactArgs("APP"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if err := instanceGiven(cmd, instance); err != nil {
				return err
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			var in *apiv1.AppInstance
			if instance != "" {
				if _, in, err = g.appInstance(cmd, c, app, instance); err != nil {
					return err
				}
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			s, err := c.ListAppSecrets(ctx, app)
			if err != nil {
				return secretError(err, app)
			}
			if in == nil {
				return show(g, s, func(s *apiv1.AppSecrets) string { return renderSecrets(app, s) })
			}
			e := instanceSecrets(s, in.Name)
			return show(g, e, func(e *effectiveSecrets) string { return renderEffective(e, in) })
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", instanceUsage)
	return cmd
}

func (g *globals) appSecretGetCmd() *cobra.Command {
	var instance string
	cmd := &cobra.Command{
		Use:   "get APP NAME",
		Short: "Print one secret's value (role operator)",
		Long: `Prints the value of the app-wide secret NAME followed by a line break, and
nothing else, so that it can be piped or captured: VALUE=$(charlie app secret
get APP NAME). With --instance it prints the value that instance gets: its
override, or else the app-wide value, which it says on stderr. --json prints
the name, the value, the instance whose override it is (none for an app-wide
value) and when and by whom it was set. When stdout is a terminal it warns on
stderr that the value is on the screen. The platform records every read in the
audit log, without the value.`,
		Args: exactArgs("APP", "NAME"),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, name := args[0], args[1]
			if err := checkSecretName(name); err != nil {
				return usagef(cmd, "%v", err)
			}
			if err := instanceGiven(cmd, instance); err != nil {
				return err
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			inName := ""
			if instance != "" {
				_, in, err := g.appInstance(cmd, c, app, instance)
				if err != nil {
					return err
				}
				inName = in.Name
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			v, err := c.GetAppSecret(ctx, app, name, inName)
			if inName != "" && client.ErrorCode(err) == "secret_not_found" {
				// No override: the instance gets the app-wide value.
				var appErr error
				v, appErr = c.GetAppSecret(ctx, app, name, "")
				switch {
				case client.ErrorCode(appErr) == "secret_not_found":
					return fmt.Errorf("%w\nThere is no app-wide %s either; see what instance %s gets with: charlie app secret list %s --instance %s", err, name, inName, app, inName)
				case appErr == nil:
					fmt.Fprintf(g.app.Stderr, "note: instance %s does not override %s; this is the app-wide value it inherits\n", inName, name)
				}
				err = appErr
			}
			if err != nil {
				return secretError(err, app)
			}
			if isTerminal(g.app.Stdout) {
				fmt.Fprintf(g.app.Stderr, "warning: the value of %s is shown on the screen\n", name)
			}
			if g.json {
				return g.out().json(v)
			}
			_, err = io.WriteString(g.app.Stdout, v.Value+"\n") // as it is: no colors
			return err
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", instanceUsage)
	return cmd
}

func (g *globals) appSecretSetCmd() *cobra.Command {
	var (
		instance, fromFile, envFile string
		fromStdin                   bool
	)
	cmd := &cobra.Command{
		Use:   "set APP NAME",
		Short: "Set secrets app-wide or for one instance (role operator)",
		Long: `Sets the app-wide secret NAME, or with --instance that instance's override of
it. The value is never taken from the command line, where it would stay in
the shell history: by default charlie asks for it without showing what you
type (in Git Bash's mintty, run charlie with winpty or pipe the value);
--stdin reads one line piped to stdin (the line break at its end is dropped,
everything else is kept); --from-file reads a file as it is, without one line
break at its end.

"set APP --env-file FILE" sets every NAME=value line of FILE in one change.
Blank lines and lines starting with # are skipped; the value is everything
after the first "=", quotes included. A bad line stops it with its line
number before anything is sent.

Names match ^[A-Z_][A-Z0-9_]*$, have at most 64 characters, and are not PORT,
DATABASE_URL or S3_*, which the platform sets. Values are 1 to 4096 bytes of
UTF-8 text without NUL bytes; a UTF-8 byte order mark at the start is dropped
and UTF-16 is refused. One change sets at most 50 names. A secret wins over a
platform.yaml env entry of the same name. Running instances keep the old
values until they are deployed again; charlie prints the "charlie deploy
--redeploy" commands that apply them without a build.`,
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0:
				return usagef(cmd, "%s needs APP NAME, or APP --env-file FILE", cmd.CommandPath())
			case len(args) > 2:
				return usagef(cmd, "%s takes APP NAME only: %s", cmd.CommandPath(), noValueArg)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			fl := cmd.Flags()
			sources := 0
			for _, given := range []bool{fromStdin, fl.Changed("from-file"), fl.Changed("env-file")} {
				if given {
					sources++
				}
			}
			switch {
			case sources > 1:
				return usagef(cmd, "use only one of --stdin, --from-file and --env-file")
			case fl.Changed("from-file") && fromFile == "":
				return usagef(cmd, "--from-file needs a file")
			case fl.Changed("env-file") && envFile == "":
				return usagef(cmd, "--env-file needs a file")
			case envFile != "" && len(args) == 2:
				return usagef(cmd, "--env-file takes the names from the file; leave NAME out")
			case envFile == "" && len(args) == 1:
				return usagef(cmd, "%s needs NAME, or --env-file FILE", cmd.CommandPath())
			}
			if envFile == "" {
				if strings.Contains(args[1], "=") {
					return usagef(cmd, "NAME has no \"=\": %s", noValueArg)
				}
				if err := checkSecretName(args[1]); err != nil {
					return usagef(cmd, "%v", err)
				}
			}
			switch {
			case sources == 0 && !isTerminal(g.app.Stdin):
				return usagef(cmd, "stdin is not a terminal, so charlie cannot ask for the value; pass --stdin to read it from stdin, or --from-file FILE")
			case fromStdin && isTerminal(g.app.Stdin):
				return usagef(cmd, "--stdin is for a piped value, but stdin is a terminal, where the value would show as you type; leave out --stdin to be asked without echo, or pipe the value in")
			}
			if err := instanceGiven(cmd, instance); err != nil {
				return err
			}
			// What is not typed is read and checked before anything is sent.
			var (
				set map[string]string
				err error
			)
			switch {
			case envFile != "":
				if set, err = readEnvFile(envFile); err != nil {
					return err
				}
			case sources > 0:
				v, err := g.readSecretValue(cmd, args[1], fromStdin, fromFile)
				if err != nil {
					return err
				}
				set = map[string]string{args[1]: v}
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			a, in, err := g.appInstance(cmd, c, app, instance)
			if err != nil {
				return err
			}
			if set == nil { // asked for once the target is known
				v, err := g.readSecretValue(cmd, args[1], false, "")
				if err != nil {
					return err
				}
				set = map[string]string{args[1]: v}
			}
			req := apiv1.AppSecretsUpdate{Set: &set}
			if in != nil {
				req.Instance = &in.Name
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			after, err := c.UpdateAppSecrets(ctx, app, req)
			if err != nil {
				return secretError(err, app)
			}
			names := slices.Sorted(maps.Keys(set))
			return g.secretsChanged(a, in, after, names, fmt.Sprintf("Set %s %s.\n", strings.Join(names, ", "), scopeText(app, in)))
		},
	}
	f := cmd.Flags()
	f.StringVar(&instance, "instance", "", instanceUsage)
	f.BoolVar(&fromStdin, "stdin", false, "read the value from one line of stdin")
	f.StringVar(&fromFile, "from-file", "", "read the value from FILE, without one line break at its end")
	f.StringVar(&envFile, "env-file", "", "set every NAME=value line of FILE in one change")
	return cmd
}

func (g *globals) appSecretRmCmd() *cobra.Command {
	var (
		instance string
		yes      bool
	)
	cmd := &cobra.Command{
		Use:   "rm APP NAME...",
		Short: "Delete secrets app-wide or from one instance (role operator)",
		Long: `Deletes the app-wide secrets NAME... in one change, or with --instance that
instance's overrides, after which it gets the app-wide values again. The
values cannot be recovered. On a terminal it asks you to type yes unless --yes
is given; without a terminal --yes is required. Names that are not set are
skipped with a note. Running instances keep the old values until they are
deployed again; charlie prints the "charlie deploy --redeploy" commands that
apply the change without a build.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return usagef(cmd, "%s needs APP NAME...", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			app, names := args[0], args[1:]
			if len(names) > maxSecretNames {
				return usagef(cmd, "%d names; one change deletes at most %d", len(names), maxSecretNames)
			}
			for i, n := range names {
				if err := checkSecretName(n); err != nil {
					return usagef(cmd, "%v", err)
				}
				if slices.Contains(names[:i], n) {
					return usagef(cmd, "%s is given twice", n)
				}
			}
			if err := instanceGiven(cmd, instance); err != nil {
				return err
			}
			if !yes && !isTerminal(g.app.Stdin) {
				return usagef(cmd, "stdin is not a terminal, so charlie cannot ask; pass --yes to delete without asking")
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			a, in, err := g.appInstance(cmd, c, app, instance)
			if err != nil {
				return err
			}
			inName := ""
			if in != nil {
				inName = in.Name
			}
			ctx, cancel := callCtx(cmd)
			now, err := c.ListAppSecrets(ctx, app)
			cancel()
			if err != nil {
				return secretError(err, app)
			}
			set := scopeNames(now, inName)
			var del []string
			for _, n := range names {
				if slices.Contains(set, n) {
					del = append(del, n)
				} else {
					fmt.Fprintf(g.app.Stderr, "note: %s is not set %s; skipping it\n", n, scopeText(app, in))
				}
			}
			if len(del) == 0 {
				return fmt.Errorf("nothing to delete; see the secrets with: charlie app secret list %s", app)
			}
			if !yes {
				prompt := fmt.Sprintf("This deletes %s %s. The values cannot be recovered.\nType yes to confirm: ", strings.Join(del, ", "), scopeText(app, in))
				ok, err := g.confirm(prompt, "yes")
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: ExitError, msg: "nothing was deleted; type yes to confirm, or pass --yes"}
				}
			}
			req := apiv1.AppSecretsUpdate{Delete: &del}
			if in != nil {
				req.Instance = &in.Name
			}
			ctx, cancel = callCtx(cmd)
			defer cancel()
			after, err := c.UpdateAppSecrets(ctx, app, req)
			if err != nil {
				return secretError(err, app)
			}
			return g.secretsChanged(a, in, after, del, fmt.Sprintf("Deleted %s %s.\n", strings.Join(del, ", "), scopeText(app, in)))
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", instanceUsage)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return cmd
}

// instanceGiven refuses an empty --instance, which would mean app-wide.
func instanceGiven(cmd *cobra.Command, instance string) error {
	if cmd.Flags().Changed("instance") && instance == "" {
		return usagef(cmd, "--instance needs an instance name or a branch")
	}
	return nil
}

// appInstance reads a registered app and, when arg is set, finds the
// instance it names.
func (g *globals) appInstance(cmd *cobra.Command, c *client.Client, app, arg string) (*apiv1.App, *apiv1.AppInstance, error) {
	ctx, cancel := callCtx(cmd)
	defer cancel()
	a, err := c.GetApp(ctx, app)
	if err != nil {
		return nil, nil, err
	}
	if a.Deploy == nil {
		return nil, nil, fmt.Errorf("app %s is only in the server's apps file, not registered from GitHub; it has no secrets", app)
	}
	if arg == "" {
		return a, nil, nil
	}
	in, err := findInstance(app, a.Deploy.Instances, arg, false)
	return a, in, err
}

// checkSecretName returns why name cannot be a secret's name, as the server
// would. A name with "=" is not printed: the rest may be a value.
func checkSecretName(name string) error {
	switch {
	case strings.Contains(name, "="):
		return errors.New(`a secret name has no "="`)
	case len(name) > maxSecretName:
		return fmt.Errorf("secret name %.20q... is longer than %d characters", name, maxSecretName)
	case !secretNamePattern.MatchString(name):
		return fmt.Errorf("secret name %q must match ^[A-Z_][A-Z0-9_]*$", name)
	case appspec.Reserved(name):
		return fmt.Errorf("secret name %s is set by the platform (PORT, DATABASE_URL and S3_* are reserved)", name)
	}
	return nil
}

// checkSecretValue returns why v cannot be the value of name, never
// printing v.
func checkSecretValue(name string, v []byte) error {
	switch {
	case len(v) == 0:
		return fmt.Errorf("the value of %s is empty", name)
	case len(v) > maxSecretValue:
		return fmt.Errorf("the value of %s is longer than %d bytes", name, maxSecretValue)
	case bytes.IndexByte(v, 0) >= 0:
		return fmt.Errorf("the value of %s has a NUL byte, which an environment variable cannot hold", name)
	case !utf8.Valid(v):
		return fmt.Errorf("the value of %s is not UTF-8 text", name)
	}
	return nil
}

// plainText drops a UTF-8 byte order mark from the start of b. It reports
// false for UTF-16, which Windows PowerShell 5.1 writes by default.
func plainText(b []byte) ([]byte, bool) {
	if bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || bytes.HasPrefix(b, []byte{0xFE, 0xFF}) {
		return nil, false
	}
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}), true
}

// readSecretValue reads the value of name from a file, from one line of
// stdin or from the terminal without echo, and checks it.
func (g *globals) readSecretValue(cmd *cobra.Command, name string, fromStdin bool, file string) (string, error) {
	var (
		b   []byte
		err error
	)
	// Enough for a byte order mark, the longest value, \r\n and one byte more.
	const limit = 3 + maxSecretValue + 2 + 1
	switch {
	case file != "":
		b, err = readUpTo(file, limit)
		if err != nil {
			return "", err
		}
		var ok bool
		if b, ok = plainText(b); !ok {
			return "", fmt.Errorf("%s is UTF-16; save it as UTF-8", file)
		}
		b = trimLineBreak(b)
		if len(b) > maxSecretValue {
			return "", fmt.Errorf("%s holds more than %d bytes, the most a secret value can have", file, maxSecretValue)
		}
	case fromStdin:
		b, err = io.ReadAll(io.LimitReader(g.app.Stdin, limit))
		if err != nil {
			return "", fmt.Errorf("read the value from stdin: %w", err)
		}
		var ok bool
		if b, ok = plainText(b); !ok {
			return "", errors.New("stdin is UTF-16; send the value as UTF-8")
		}
		b = trimLineBreak(b)
		if slices.Contains(b, '\n') {
			return "", errors.New("--stdin reads one line, and stdin has more; use --from-file for a value of several lines")
		}
	default:
		fmt.Fprintf(g.app.Stderr, "Value of %s (not shown): ", name)
		b, err = readHidden(cmd.Context(), g.app.Stdin)
		fmt.Fprintln(g.app.Stderr)
		if ctxErr := cmd.Context().Err(); ctxErr != nil { // Ctrl-C, even if a line came after it
			return "", ctxErr
		}
		if err != nil {
			return "", fmt.Errorf("read the value: %w", err)
		}
	}
	if err := checkSecretValue(name, b); err != nil {
		return "", fmt.Errorf("%w; nothing was set", err)
	}
	return string(b), nil
}

// trimLineBreak drops one line break, \n or \r\n, from the end of b.
func trimLineBreak(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}

// readUpTo reads at most limit bytes of a file.
func readUpTo(name string, limit int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// readEnvFile reads the NAME=value lines of an --env-file.
func readEnvFile(name string) (map[string]string, error) {
	data, err := readUpTo(name, maxEnvFile+1)
	if err != nil {
		return nil, err
	}
	if len(data) > maxEnvFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxEnvFile)
	}
	data, ok := plainText(data)
	if !ok {
		return nil, fmt.Errorf("%s is UTF-16; save it as UTF-8", name)
	}
	return parseEnvFile(name, data)
}

// parseEnvFile takes NAME=value lines, skipping blank lines and # comments.
// Errors name the bad lines by number and never print a value, nor the
// text before "=" unless it is a well-formed name: on a line that is not
// NAME=value, such as "ADMIN_TOKEN: dGhp...==" from YAML or a connection
// string, that text may hold a secret.
func parseEnvFile(file string, data []byte) (map[string]string, error) {
	set := map[string]string{}
	seen := map[string]int{} // name to line
	var bad []string
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		line = strings.TrimSuffix(line, "\r")
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			bad = append(bad, fmt.Sprintf("line %d: want NAME=value", n))
			continue
		}
		switch {
		case !secretNamePattern.MatchString(name):
			bad = append(bad, fmt.Sprintf(`line %d: not NAME=value: the text before the first "=" does not match ^[A-Z_][A-Z0-9_]*$`, n))
			continue
		case len(name) > maxSecretName:
			bad = append(bad, fmt.Sprintf("line %d: the name is longer than %d characters", n, maxSecretName))
			continue
		case appspec.Reserved(name):
			bad = append(bad, fmt.Sprintf("line %d: %v", n, checkSecretName(name)))
			continue
		}
		if prev, dup := seen[name]; dup {
			bad = append(bad, fmt.Sprintf("line %d: %s is already set on line %d", n, name, prev))
			continue
		}
		seen[name] = n
		if err := checkSecretValue(name, []byte(value)); err != nil {
			bad = append(bad, fmt.Sprintf("line %d: %v", n, err))
			continue
		}
		set[name] = value
	}
	switch {
	case len(bad) > 0:
		return nil, fmt.Errorf("%s: nothing was set:\n  %s", file, strings.Join(bad, "\n  "))
	case len(set) == 0:
		return nil, fmt.Errorf("%s has no NAME=value lines; nothing was set", file)
	case len(set) > maxSecretNames:
		return nil, fmt.Errorf("%s sets %d secrets; one change sets at most %d", file, len(set), maxSecretNames)
	}
	return set, nil
}

// secretError adds what to do to errors of the secret API.
func secretError(err error, app string) error {
	switch client.ErrorCode(err) {
	case "secrets_not_configured":
		return fmt.Errorf("%w\nApp secrets are off on this server until an administrator gives it a secrets key (PLATFORM_SECRETS_KEY).", err)
	case "secret_not_found":
		return fmt.Errorf("%w\nSee the secrets with: charlie app secret list %s", err, app)
	case "instance_not_found":
		return fmt.Errorf("%w\nSee the instances with: charlie app branch list %s", err, app)
	}
	return err
}

// scopeText says where a change goes: app-wide, or one instance.
func scopeText(app string, in *apiv1.AppInstance) string {
	if in == nil {
		return "app-wide in app " + app
	}
	return fmt.Sprintf("for instance %s of app %s", in.Name, app)
}

// scopeNames lists the app-wide secrets, or one instance's overrides.
func scopeNames(s *apiv1.AppSecrets, instance string) []string {
	infos := s.App
	if instance != "" {
		infos = nil
		for _, in := range s.Instances {
			if in.Instance == instance {
				infos = in.Secrets
			}
		}
	}
	names := make([]string, len(infos))
	for i, si := range infos {
		names[i] = si.Name
	}
	return names
}

// secretsChanged prints done and how to apply the change: running
// instances keep the old values until they are deployed again. --json
// prints the names after the change instead and the rest on stderr.
func (g *globals) secretsChanged(a *apiv1.App, in *apiv1.AppInstance, after *apiv1.AppSecrets, names []string, done string) error {
	note := redeployNote(a, in, after, names)
	if g.json {
		if err := g.out().json(after); err != nil {
			return err
		}
		_, err := io.WriteString(g.app.Stderr, note)
		return err
	}
	return g.out().text(done + note)
}

// redeployNote lists the redeploy commands of the instances the change
// reaches: the one instance, or for app-wide names every instance that does
// not override them all.
func redeployNote(a *apiv1.App, in *apiv1.AppInstance, after *apiv1.AppSecrets, names []string) string {
	var targets []apiv1.AppInstance
	if in != nil {
		targets = []apiv1.AppInstance{*in}
	} else {
		ins := a.Deploy.Instances
		if len(ins) == 0 { // a server from before branch instances
			ins = []apiv1.AppInstance{{Name: a.Name, Branch: a.Deploy.Branch, Default: true}}
		}
		for _, i := range ins {
			own := scopeNames(after, i.Name)
			if slices.ContainsFunc(names, func(n string) bool { return !slices.Contains(own, n) }) {
				targets = append(targets, i)
			}
		}
	}
	if len(targets) == 0 {
		return "Every instance overrides these secrets, so none of them gets the app-wide values.\n"
	}
	var b strings.Builder
	b.WriteString("Running instances keep the old values until they are deployed again; to apply the change now, without a build:\n")
	for _, t := range targets {
		cmd := "charlie deploy " + a.Name + " --redeploy"
		if !t.Default {
			cmd += " --branch " + t.Branch
		}
		b.WriteString("  " + cmd + "\n")
	}
	return b.String()
}

func renderSecrets(app string, s *apiv1.AppSecrets) string {
	if len(s.App) == 0 && len(s.Instances) == 0 {
		return fmt.Sprintf("No secrets in app %s. Set one with: charlie app secret set %s NAME\n", app, app)
	}
	var b strings.Builder
	if len(s.App) == 0 {
		b.WriteString("No app-wide secrets.\n")
	} else {
		b.WriteString("App-wide, for every instance that does not override them\n" + secretTable(s.App))
	}
	for _, in := range s.Instances {
		b.WriteString("\nOverrides in instance " + in.Instance + "\n" + secretTable(in.Secrets))
	}
	return b.String()
}

func secretTable(infos []apiv1.AppSecretInfo) string {
	rows := [][]string{{"NAME", "UPDATED", "BY"}}
	for _, s := range infos {
		rows = append(rows, []string{s.Name, stamp(s.UpdatedAt), s.UpdatedBy})
	}
	return table(rows)
}

// effectiveSecrets is what "app secret list --instance" prints: the
// secrets one instance gets.
type effectiveSecrets struct {
	Instance string            `json:"instance"`
	Secrets  []effectiveSecret `json:"secrets"`
}

type effectiveSecret struct {
	Name      string    `json:"name"`
	Source    string    `json:"source"` // app, override or instance-only
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
}

// instanceSecrets is the app-wide secrets with the instance's overrides on
// top, by name.
func instanceSecrets(s *apiv1.AppSecrets, instance string) *effectiveSecrets {
	byName := map[string]effectiveSecret{}
	for _, si := range s.App {
		byName[si.Name] = effectiveSecret{Name: si.Name, Source: "app", UpdatedAt: si.UpdatedAt, UpdatedBy: si.UpdatedBy}
	}
	for _, in := range s.Instances {
		if in.Instance != instance {
			continue
		}
		for _, si := range in.Secrets {
			source := "instance-only"
			if _, ok := byName[si.Name]; ok {
				source = "override"
			}
			byName[si.Name] = effectiveSecret{Name: si.Name, Source: source, UpdatedAt: si.UpdatedAt, UpdatedBy: si.UpdatedBy}
		}
	}
	e := &effectiveSecrets{Instance: instance, Secrets: make([]effectiveSecret, 0, len(byName))}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		e.Secrets = append(e.Secrets, byName[name])
	}
	return e
}

func renderEffective(e *effectiveSecrets, in *apiv1.AppInstance) string {
	if len(e.Secrets) == 0 {
		return fmt.Sprintf("Instance %s gets no secrets.\n", e.Instance)
	}
	rows := [][]string{{"NAME", "SOURCE", "UPDATED", "BY"}}
	for _, s := range e.Secrets {
		rows = append(rows, []string{s.Name, s.Source, stamp(s.UpdatedAt), s.UpdatedBy})
	}
	return fmt.Sprintf("Secrets of instance %s (branch %s)\n%s", in.Name, in.Branch, table(rows))
}
