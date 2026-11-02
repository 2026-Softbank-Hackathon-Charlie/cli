package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

func (g *globals) agentCmd() *cobra.Command {
	return group("agent", "The AI agent's sessions: its tool calls and their results",
		g.agentListCmd(), g.agentShowCmd(), g.agentWatchCmd())
}

func (g *globals) agentListCmd() *cobra.Command {
	var (
		planRef, runID string
		limit          int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agent sessions, newest first",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("limit") && (limit < 1 || limit > 200) {
				return usagef(cmd, "--limit must be between 1 and 200")
			}
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			l, err := c.ListAgentSessions(ctx, client.AgentSessionFilter{PlanRef: planRef, RunID: runID, Limit: limit})
			if err != nil {
				return err
			}
			return show(g, l, renderAgentList)
		},
	}
	f := cmd.Flags()
	f.StringVar(&planRef, "plan", "", "only sessions about this plan ref")
	f.StringVar(&runID, "run", "", "only sessions about this run id")
	f.IntVar(&limit, "limit", 0, "sessions to show, 1 to 200 (server default 50)")
	return cmd
}

// agentDetail is what "agent show --json" prints.
type agentDetail struct {
	Session apiv1.AgentSession `json:"session"`
	Events  []apiv1.AgentEvent `json:"events"`
}

func (g *globals) agentShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
		Short: "Show one agent session and every event it recorded",
		Args:  exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := callCtx(cmd)
			defer cancel()
			s, err := c.GetAgentSession(ctx, args[0])
			if err != nil {
				return err
			}
			evs, err := c.ListAgentEvents(ctx, args[0], 0)
			if err != nil {
				return err
			}
			return show(g, &agentDetail{Session: *s, Events: evs.Events}, renderAgentDetail)
		},
	}
}

func (g *globals) agentWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch ID",
		Short: "Follow an agent session's tool calls and results live",
		Long: `Prints each tool call, tool result, message, blocked action, decision and error
as the agent records it, until the session finishes. A dropped connection is
resumed from the last event received. Exits 0 when the session succeeded and 3
when it failed or was cancelled.`,
		Args: exactArgs("ID"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.client(cmd)
			if err != nil {
				return err
			}
			return g.watchAgent(cmd, c, args[0])
		},
	}
}

// watchAgent follows a session and turns its final status into the exit code.
func (g *globals) watchAgent(cmd *cobra.Command, c *client.Client, id string) error {
	ctx := cmd.Context()
	p := g.out()
	var last *apiv1.AgentSession
	err := c.WatchAgentSession(ctx, id, func(m client.AgentMessage) error {
		first := last == nil
		if m.Snapshot != nil {
			last = m.Snapshot
		}
		if g.json {
			if m.Snapshot != nil {
				return p.jsonLine(jsonMessage{Event: m.Name, Data: m.Snapshot})
			}
			return p.jsonLine(jsonMessage{Event: m.Name, ID: m.Seq, Data: m.Event})
		}
		switch {
		case m.Event != nil:
			return p.text(agentEventLine(m.Event) + "\n")
		case first:
			return p.text(sessionHeader(m.Snapshot) + "\n")
		case m.Snapshot.Status == apiv1.AgentSessionStatusRunning:
			return p.text(clock(time.Now()) + "  reconnected: " + agentMark(m.Snapshot.Status) + " running\n")
		}
		return p.text("\n" + sessionEnd(m.Snapshot))
	})
	if err != nil {
		if ctx.Err() != nil {
			return &exitError{code: ExitInterrupted, msg: fmt.Sprintf(
				"interrupted; the agent session goes on. Reattach with: charlie agent watch %s", id)}
		}
		return err
	}
	if last == nil || last.Status == apiv1.AgentSessionStatusSucceeded {
		return nil
	}
	msg := fmt.Sprintf("agent session %s ended %s", id, last.Status)
	if s := str(last.Summary); s != "" {
		msg += ": " + s
	}
	return &exitError{code: ExitRunFailed, msg: msg}
}

func agentMark(s apiv1.AgentSessionStatus) string {
	switch s {
	case apiv1.AgentSessionStatusSucceeded:
		return "[ok]"
	case apiv1.AgentSessionStatusRunning:
		return "[..]"
	}
	return "[x]"
}

// failedStatus reports event statuses that mean the tool call did not work.
func failedStatus(s string) bool {
	switch strings.ToLower(s) {
	case "error", "failed", "denied", "forbidden", "timeout":
		return true
	}
	return false
}

func eventMark(e *apiv1.AgentEvent) string {
	switch e.Type {
	case apiv1.AgentEventTypeToolCall:
		return "[..]"
	case apiv1.AgentEventTypeToolResult:
		if failedStatus(str(e.Status)) {
			return "[x]"
		}
		return "[ok]"
	case apiv1.AgentEventTypeBlocked, apiv1.AgentEventTypeError:
		return "[x]"
	}
	return ""
}

// payload renders an event's input or output on one line: a JSON string as
// its text, anything else as compact JSON.
func payload(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return clip(s, 160)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return clip(string(b), 160)
}

// agentEventLine is one human line for an event.
func agentEventLine(e *apiv1.AgentEvent) string {
	parts := []string{eventMark(e), string(e.Type), str(e.Tool), str(e.Status)}
	if e.DurationMillis != nil {
		parts = append(parts, millis(e.DurationMillis))
	}
	body := e.Output
	if e.Type == apiv1.AgentEventTypeToolCall || body == nil {
		body = e.Input
	}
	parts = append(parts, payload(body))
	if u := e.Usage; u != nil {
		parts = append(parts, fmt.Sprintf("(%d/%d tokens)", u.InputTokens, u.OutputTokens))
	}
	return clock(e.Time) + "  " + strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

func sessionHeader(s *apiv1.AgentSession) string {
	who := s.Actor
	if m := str(s.Model); m != "" {
		who += " (" + m + ")"
	}
	return fmt.Sprintf("Agent session %s  %s  %q  by %s: %s %s", s.Id, s.Kind, s.Title, who, agentMark(s.Status), s.Status)
}

func sessionEnd(s *apiv1.AgentSession) string {
	out := fmt.Sprintf("Agent session %s ended %s %s", s.Id, agentMark(s.Status), s.Status)
	if sum := str(s.Summary); sum != "" {
		out += ": " + sum
	}
	return out + "\n" + totals(s) + "\n"
}

func totals(s *apiv1.AgentSession) string {
	t := s.Totals
	return fmt.Sprintf("%d events, %d tool calls, %d/%d tokens, %s in tools, %s in all",
		t.Events, t.ToolCalls, t.InputTokens, t.OutputTokens, human(time.Duration(t.DurationMillis)*time.Millisecond), wallClock(s))
}

// wallClock is how long a session took, or has been running.
func wallClock(s *apiv1.AgentSession) string {
	if s.EndedAt != nil {
		return human(s.EndedAt.Sub(s.CreatedAt))
	}
	return human(time.Since(s.CreatedAt))
}

func renderAgentList(l *apiv1.AgentSessionList) string {
	rows := [][]string{{"ID", "KIND", "STATUS", "TITLE", "ACTOR", "MODEL", "EVENTS", "TOOL CALLS", "TOKENS", "STARTED", "DURATION"}}
	for _, s := range l.Sessions {
		rows = append(rows, []string{s.Id, string(s.Kind), agentMark(s.Status) + " " + string(s.Status), clip(s.Title, 60), s.Actor, str(s.Model),
			fmt.Sprint(s.Totals.Events), fmt.Sprint(s.Totals.ToolCalls), fmt.Sprintf("%d/%d", s.Totals.InputTokens, s.Totals.OutputTokens),
			stamp(s.CreatedAt), wallClock(&s)})
	}
	return table(rows)
}

func renderAgentDetail(d *agentDetail) string {
	s := &d.Session
	out := fields([][2]string{
		{"Session", s.Id},
		{"Kind", string(s.Kind)},
		{"Title", s.Title},
		{"Status", agentMark(s.Status) + " " + string(s.Status)},
		{"Actor", s.Actor},
		{"Model", str(s.Model)},
		{"Plan", str(s.PlanRef)},
		{"Run", str(s.RunId)},
		{"Started", stamp(s.CreatedAt)},
		{"Ended", stampPtr(s.EndedAt)},
		{"Summary", str(s.Summary)},
		{"Totals", totals(s)},
	})
	if len(d.Events) == 0 {
		return out + "\nNo events.\n"
	}
	rows := [][]string{{"SEQ", "TIME", "TYPE", "TOOL", "STATUS", "DURATION", "INPUT", "OUTPUT"}}
	for i := range d.Events {
		e := &d.Events[i]
		rows = append(rows, []string{fmt.Sprint(e.Seq), clock(e.Time), strings.TrimSpace(eventMark(e) + " " + string(e.Type)), str(e.Tool), str(e.Status),
			millis(e.DurationMillis), clip(payload(e.Input), 60), clip(payload(e.Output), 60)})
	}
	return out + "\n" + table(rows)
}
