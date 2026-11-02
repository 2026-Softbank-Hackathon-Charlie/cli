package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

func (g *globals) trafficCmd() *cobra.Command {
	var (
		watch    bool
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "traffic",
		Short: "Show the traffic gateway's routing and request statistics",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval <= 0 {
				return usagef(cmd, "--interval must be positive")
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			if watch {
				return g.pollTraffic(cmd, c, interval)
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			s, err := c.GetTraffic(ctx)
			if err != nil {
				return err
			}
			return show(g, s, renderTraffic)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "refresh until interrupted")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "refresh interval for --watch")
	return cmd
}

// pollTraffic refreshes until Ctrl-C, which ends it successfully. Server and
// network errors are shown and retried; 4xx answers stop it.
func (g *globals) pollTraffic(cmd *cobra.Command, c *client.Client, interval time.Duration) error {
	ctx := cmd.Context()
	p := g.out()
	tty := !g.json && ansiTerminal(g.app.Stdout)
	scr := &screen{w: g.app.Stdout}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, requestTimeout)
		s, err := c.GetTraffic(cctx)
		cancel()
		var ae *client.APIError
		var werr error
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.As(err, &ae) && ae.Status < 500:
			return err
		case err != nil && tty:
			scr.draw(fmt.Sprintf("%s  error: %v (retrying)\n", clock(time.Now()), err))
		case err != nil:
			fmt.Fprintf(g.app.Stderr, "%s  error: %v (retrying)\n", clock(time.Now()), err)
		case g.json:
			werr = p.jsonLine(s)
		case tty:
			cols, rows := termSize(g.app.Stdout)
			scr.draw(p.paint(fit(renderTraffic(s), cols, rows-1)))
		default:
			werr = p.text(renderTraffic(s) + "\n")
		}
		if werr != nil {
			return werr // stdout is gone
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// backendNames lists the gateway's backends first, then any name that only
// appears in the weights or statistics.
func backendNames(s *apiv1.TrafficState) []string {
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, b := range s.Backends {
		add(b.Name)
	}
	var extra []string
	for n := range s.ReadWeights {
		extra = append(extra, n)
	}
	for n := range s.Stats {
		extra = append(extra, n)
	}
	for n := range s.Totals {
		extra = append(extra, n)
	}
	slices.Sort(extra)
	for _, n := range extra {
		add(n)
	}
	return names
}

func latency(ms float32) string {
	return human(time.Duration(float64(ms) * float64(time.Millisecond)))
}

func renderTraffic(s *apiv1.TrafficState) string {
	held := "no"
	if s.WritesHeld {
		held = fmt.Sprintf("yes, %d waiting", s.HeldWrites)
		if s.HeldSince != nil {
			held += " since " + clock(*s.HeldSince)
		}
		held += ", released after " + s.MaxHold
	}
	out := fields([][2]string{
		{"Write backend", s.WriteBackend},
		{"Writes held", held},
		{"In flight", fmt.Sprintf("%d reads, %d writes", s.InFlight.Reads, s.InFlight.Writes)},
		{"Updated", stamp(s.UpdatedAt)},
	})
	health := map[string]apiv1.BackendState{}
	for _, b := range s.Backends {
		health[b.Name] = b
	}
	rows := [][]string{{"BACKEND", "HEALTH", "WRITES", "READS", "RPS", "ERRORS", "P50", "P95", "P99", "WINDOW", "TOTAL", "URL"}}
	var problems []string
	for _, n := range backendNames(s) {
		row := []string{n, "", "", "", "", "", "", "", "", "", "", ""}
		if b, ok := health[n]; ok {
			row[1], row[11] = "[x] down", b.Url
			if b.Healthy {
				row[1] = "[ok] up"
			}
			if b.LastError != nil && *b.LastError != "" {
				problems = append(problems, fmt.Sprintf("%s: %s", n, *b.LastError))
			}
		}
		if n == s.WriteBackend {
			row[2] = "yes"
		}
		if w, ok := s.ReadWeights[n]; ok {
			row[3] = fmt.Sprintf("%d%%", w)
		}
		if st, ok := s.Stats[n]; ok {
			row[4] = fmt.Sprintf("%.1f", st.Rps)
			row[5] = fmt.Sprintf("%d of %d (%s)", st.Errors, st.Requests, percent(st.ErrorRate))
			row[6], row[7], row[8] = latency(st.P50Millis), latency(st.P95Millis), latency(st.P99Millis)
			row[9] = fmt.Sprintf("%ds", st.WindowSeconds)
		}
		if t, ok := s.Totals[n]; ok {
			row[10] = fmt.Sprintf("%d req, %d err", t.Requests, t.Errors)
		}
		rows = append(rows, row)
	}
	out += "\n" + table(rows)
	for _, p := range problems {
		out += p + "\n"
	}
	return out
}
