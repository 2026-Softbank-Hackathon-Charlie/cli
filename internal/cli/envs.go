package cli

import (
	"fmt"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/spf13/cobra"
)

func (g *globals) envCmd() *cobra.Command {
	list := &cobra.Command{
		Use:   "list",
		Short: "List environments with their live cluster status",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListEnvironments(ctx)
			if err != nil {
				return err
			}
			return show(g, l, renderEnvList)
		},
	}
	showCmd := &cobra.Command{
		Use:   "show NAME",
		Short: "Show one environment",
		Args:  exactArgs("NAME"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			e, err := c.GetEnvironment(ctx, args[0])
			if err != nil {
				return err
			}
			return show(g, e, renderEnv)
		},
	}
	return group("env", "Environments (clusters the platform can migrate between)", list, showCmd)
}

func reachable(s apiv1.EnvironmentStatus) string {
	if s.Reachable {
		return "[ok] reachable"
	}
	return "[x] unreachable"
}

// live marks the environment the gateway sends writes to.
func live(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func renderEnvList(l *apiv1.EnvironmentList) string {
	rows := [][]string{{"NAME", "PROVIDER", "DISPLAY NAME", "REGION", "STATUS", "LIVE", "K8S", "NODES", "DATABASES"}}
	for _, e := range l.Environments {
		nodes := ""
		if n := e.Status.Nodes; n != nil {
			nodes = fmt.Sprintf("%d/%d", n.Ready, n.Total)
		}
		var dbs []string
		if e.Status.Databases != nil {
			for _, d := range *e.Status.Databases {
				dbs = append(dbs, fmt.Sprintf("%s %d/%d%s", d.Name, d.ReadyInstances, d.Instances, replicaNote(d.Replica)))
			}
		}
		rows = append(rows, []string{e.Name, string(e.Provider), e.DisplayName, str(e.Region), reachable(e.Status),
			live(e.Live), str(e.Status.KubernetesVersion), nodes, strings.Join(dbs, ", ")})
	}
	return table(rows)
}

func replicaNote(replica bool) string {
	if replica {
		return " replica"
	}
	return ""
}

func renderEnv(e *apiv1.Environment) string {
	st := e.Status
	writes := "no"
	if e.Live {
		writes = "yes (the gateway sends writes here)"
	}
	pairs := [][2]string{
		{"Name", e.Name},
		{"Display name", e.DisplayName},
		{"Provider", string(e.Provider)},
		{"Region", str(e.Region)},
		{"Kube context", e.KubeContext},
		{"Gateway backend", e.GatewayBackend},
		{"Receives writes", writes},
		{"Status", reachable(st) + ", checked " + stamp(st.CheckedAt)},
	}
	if st.Error != nil {
		pairs = append(pairs, [2]string{"Error", *st.Error})
	}
	pairs = append(pairs, [2]string{"Kubernetes", str(st.KubernetesVersion)})
	if n := st.Nodes; n != nil {
		pairs = append(pairs, [2]string{"Nodes", fmt.Sprintf("%d/%d ready (%s)", n.Ready, n.Total, strings.Join(n.Architectures, ", "))})
	}
	out := fields(pairs)
	if st.Databases != nil && len(*st.Databases) > 0 {
		rows := [][]string{{"DATABASE", "NAMESPACE", "PHASE", "READY", "PRIMARY", "ROLE"}}
		for _, d := range *st.Databases {
			role := "primary"
			if d.Replica {
				role = "replica"
			}
			rows = append(rows, []string{d.Name, d.Namespace, str(d.Phase), fmt.Sprintf("%d/%d", d.ReadyInstances, d.Instances), str(d.CurrentPrimary), role})
		}
		out += "\n" + table(rows)
	}
	return out
}
