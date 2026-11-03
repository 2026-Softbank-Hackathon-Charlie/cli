package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
	"github.com/spf13/cobra"
)

// deployPhases are the statuses a deployment passes through before it ends.
var deployPhases = []apiv1.DeploymentStatus{
	apiv1.DeploymentStatusQueued, apiv1.DeploymentStatusBuilding,
	apiv1.DeploymentStatusCommitting, apiv1.DeploymentStatusSyncing,
}

// deployRecent is how many event lines the live view keeps; a short
// terminal shows fewer.
const deployRecent = 30

// logsQuiet and logsMax end "deploy logs" without --follow on a deployment
// that is still going. The server sends the stored events right after the
// first snapshot, so a pause of logsQuiet means they are all in; logsMax
// bounds the wait while the build keeps logging.
var logsQuiet, logsMax = 500 * time.Millisecond, 5 * time.Second

// deployView is the latest deployment snapshot patched by the events after
// it. The server sends snapshots only at the start and the end of a stream,
// so live progress comes from the events.
type deployView struct {
	dep    apiv1.Deployment
	have   bool
	last   apiv1.DeploymentStatus // the latest phase before the end, where a failure happened
	recent []string
	url    string // the instance's public URL, read once the deployment is healthy
}

func (v *deployView) apply(m client.DeploymentMessage) {
	switch {
	case m.Snapshot != nil:
		v.dep, v.have = *m.Snapshot, true
		if !deployFinished(v.dep.Status) {
			v.last = v.dep.Status
		}
	case m.Event != nil:
		v.event(m.Event)
	}
}

func (v *deployView) event(e *apiv1.DeploymentEvent) {
	v.recent = append(v.recent, deployEventLine(e))
	if len(v.recent) > deployRecent {
		v.recent = v.recent[len(v.recent)-deployRecent:]
	}
	switch e.Type {
	case apiv1.DeploymentEventTypePhase, apiv1.DeploymentEventTypeDone:
		if e.Phase == nil {
			break
		}
		if !deployFinished(*e.Phase) {
			v.last = *e.Phase
		}
		if deployFinished(v.dep.Status) {
			break // replayed after a snapshot that already shows the end
		}
		v.dep.Status = *e.Phase
		if e.Type == apiv1.DeploymentEventTypeDone {
			at := e.Time
			v.dep.FinishedAt = &at
			if *e.Phase == apiv1.DeploymentStatusFailed && str(e.Message) != "" {
				v.dep.Error = e.Message
			}
		}
	case apiv1.DeploymentEventTypeEnvironment:
		for i := range v.dep.Environments {
			if v.dep.Environments[i].Env == str(e.Env) {
				v.dep.Environments[i].Message = e.Message
			}
		}
	}
}

// deployEventLine is one human line for an event. A log line keeps its own
// spacing.
func deployEventLine(e *apiv1.DeploymentEvent) string {
	var phase apiv1.DeploymentStatus
	if e.Phase != nil {
		phase = *e.Phase
	}
	msg := str(e.Message)
	var s string
	switch e.Type {
	case apiv1.DeploymentEventTypeLog:
		msg = strings.TrimRightFunc(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, msg), unicode.IsSpace)
		if phase == "" {
			return clock(e.Time) + "  " + msg
		}
		return clock(e.Time) + "  " + string(phase) + "  " + msg
	case apiv1.DeploymentEventTypePhase, apiv1.DeploymentEventTypeDone:
		s = deployMark(phase) + " " + string(phase)
		if msg != "" {
			s += ": " + msg
		}
	case apiv1.DeploymentEventTypeEnvironment:
		s = "env " + str(e.Env) + ": " + msg
	default:
		s = string(e.Type) + " " + msg
	}
	return clock(e.Time) + "  " + strings.Join(strings.Fields(s), " ")
}

// phaseLine marks the phases done, the current one, and where a deployment
// that did not become healthy stopped.
func phaseLine(status, last apiv1.DeploymentStatus) string {
	cur, mark := slices.Index(deployPhases, status), "[..]"
	switch {
	case status == apiv1.DeploymentStatusHealthy:
		cur = len(deployPhases)
	case deployFinished(status):
		cur, mark = slices.Index(deployPhases, last), "[x]"
	}
	parts := make([]string, 0, len(deployPhases)+1)
	for i, p := range deployPhases {
		m := "[ ]"
		switch {
		case i < cur:
			m = "[ok]"
		case i == cur:
			m = mark
		}
		parts = append(parts, m+" "+string(p))
	}
	if deployFinished(status) {
		parts = append(parts, deployMark(status)+" "+string(status))
	}
	return strings.Join(parts, "  ")
}

// liveURL is where the instance answers: its public URL, else the
// environments'.
func (v *deployView) liveURL() string {
	if v.url != "" {
		return v.url
	}
	var urls []string
	for _, e := range v.dep.Environments {
		if u := str(e.Url); u != "" && !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	return strings.Join(urls, ", ")
}

// frame renders the view. now drives the elapsed time. width and maxLines,
// when positive, make the frame fit a terminal; recent events give way first.
func (v *deployView) frame(now time.Time, width, maxLines int) string {
	d := &v.dep
	var b strings.Builder
	fmt.Fprintf(&b, "Deployment %s  %s  %s (%s)  %s by %s\n", d.Id, deployTarget(d), d.Ref, shortSHA(d.Commit), d.Trigger, d.TriggeredBy)
	fmt.Fprintf(&b, "Status %s %s  elapsed %s  created %s\n", deployMark(d.Status), d.Status, human(deployElapsed(d, now)), stamp(d.CreatedAt))
	fmt.Fprintf(&b, "Phases %s\n", phaseLine(d.Status, v.last))
	if e := str(d.Error); e != "" {
		fmt.Fprintf(&b, "Error %s\n", e)
	}
	b.WriteString("\n" + deployEnvTable(d.Environments))

	tail := ""
	if u := v.liveURL(); u != "" && d.Status == apiv1.DeploymentStatusHealthy {
		tail = "\n[ok] live at " + u + "\n"
	}
	recent := v.recent
	if maxLines > 0 {
		room := maxLines - strings.Count(b.String(), "\n") - strings.Count(tail, "\n") - 2
		recent = recent[len(recent)-max(0, min(room, len(recent))):]
	}
	if len(recent) > 0 {
		b.WriteString("\nRecent events\n" + strings.Join(recent, "\n") + "\n")
	}
	b.WriteString(tail)
	return fit(b.String(), width, maxLines)
}

// watchDeploy follows a deployment and turns its final status into the exit
// code.
func (g *globals) watchDeploy(cmd *cobra.Command, c *client.Client, id string) error {
	ctx := cmd.Context()
	v := &deployView{}
	p := g.out()
	var err error
	switch {
	case g.json:
		err = c.WatchDeployment(ctx, id, func(m client.DeploymentMessage) error {
			v.apply(m)
			return p.jsonLine(deployJSON(m))
		})
	case ansiTerminal(g.app.Stdout):
		err = g.watchDeployScreen(cmd, c, id, v, p)
	default:
		err = g.watchDeployLines(cmd, c, id, v, p)
	}
	return deployEnd(ctx, err, &v.dep, id, "charlie deploy watch "+id)
}

// deployJSON is one line of "deploy watch --json".
func deployJSON(m client.DeploymentMessage) jsonMessage {
	if m.Snapshot != nil {
		return jsonMessage{Event: m.Name, Data: m.Snapshot}
	}
	return jsonMessage{Event: m.Name, ID: m.Seq, Data: m.Event}
}

// deployEnd turns how following a deployment ended into the exit code.
func deployEnd(ctx context.Context, err error, d *apiv1.Deployment, id, reattach string) error {
	if err != nil {
		if ctx.Err() != nil {
			return &exitError{code: ExitInterrupted, msg: fmt.Sprintf(
				"interrupted; deployment %s keeps going on the server. Reattach with: %s", id, reattach)}
		}
		return err
	}
	if d.Status == apiv1.DeploymentStatusHealthy {
		return nil
	}
	msg := fmt.Sprintf("deployment %s ended %s", id, d.Status)
	if e := str(d.Error); e != "" {
		msg += ": " + e
	}
	return &exitError{code: ExitRunFailed, msg: msg}
}

// deployTarget names the app and, for an added instance, the instance a
// deployment goes to and its branch. The default instance is named after the
// app, and servers from before branch instances name none.
func deployTarget(d *apiv1.Deployment) string {
	s := "app " + d.App
	if d.Instance != "" && d.Instance != d.App {
		s += fmt.Sprintf("  instance %s (branch %s)", d.Instance, d.Branch)
	}
	return s
}

// readURL reads the instance's public URL once the deployment is healthy;
// the deployment itself knows only the environments' URLs.
func (g *globals) readURL(cmd *cobra.Command, c *client.Client, v *deployView) {
	if v.dep.Status != apiv1.DeploymentStatusHealthy {
		return
	}
	ctx, cancel := callCtx(cmd)
	defer cancel()
	if a, err := c.GetApp(ctx, v.dep.App); err == nil && a.Deploy != nil {
		v.url = instanceURL(a.Deploy, &v.dep)
	}
}

// instanceURL is the public URL of the instance d deploys. A server from
// before branch instances lists none and names no instance; its app URL is
// the one.
func instanceURL(a *apiv1.AppDeploy, d *apiv1.Deployment) string {
	for _, in := range a.Instances {
		if in.Name == d.Instance {
			return str(in.Url)
		}
	}
	if d.Instance == "" || d.Instance == d.App {
		return str(a.Url)
	}
	return ""
}

// watchDeployScreen redraws the full view on every message and once a second.
func (g *globals) watchDeployScreen(cmd *cobra.Command, c *client.Client, id string, v *deployView, p *printer) error {
	scr := &screen{w: g.app.Stdout}
	var mu sync.Mutex
	draw := func() {
		mu.Lock()
		defer mu.Unlock()
		if v.have {
			cols, rows := termSize(g.app.Stdout)
			scr.draw(p.paint(v.frame(time.Now(), cols, rows-1)))
		}
	}
	stop, ticked := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ticked)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				draw()
			}
		}
	}()
	err := c.WatchDeployment(cmd.Context(), id, func(m client.DeploymentMessage) error {
		// An environment event says that an environment changed, not to
		// what; the deployment has its status.
		var envs []apiv1.DeploymentEnvironment
		if m.Event != nil && m.Event.Type == apiv1.DeploymentEventTypeEnvironment {
			ctx, cancel := callCtx(cmd)
			if d, err := c.GetDeployment(ctx, id); err == nil {
				envs = d.Environments
			}
			cancel()
		}
		mu.Lock()
		v.apply(m)
		if envs != nil {
			v.dep.Environments = envs
		}
		mu.Unlock()
		draw()
		return nil
	})
	close(stop)
	<-ticked
	if err == nil {
		g.readURL(cmd, c, v)
	}
	draw()
	return err
}

// watchDeployLines prints one line per message, for logs and pipes, and the
// full view once the deployment has ended.
func (g *globals) watchDeployLines(cmd *cobra.Command, c *client.Client, id string, v *deployView, p *printer) error {
	first := true
	err := c.WatchDeployment(cmd.Context(), id, func(m client.DeploymentMessage) error {
		v.apply(m)
		d := &v.dep
		var line string
		switch {
		case m.Snapshot != nil && first:
			line = fmt.Sprintf("Deployment %s  %s  %s (%s) to %s: %s %s\n", d.Id, deployTarget(d), d.Ref, shortSHA(d.Commit), envNames(d.Environments), deployMark(d.Status), d.Status)
			first = false
		case m.Snapshot != nil:
			line = fmt.Sprintf("%s  snapshot: %s %s\n", clock(time.Now()), deployMark(d.Status), d.Status)
		default:
			line = deployEventLine(m.Event) + "\n"
		}
		return p.text(line)
	})
	if err == nil && v.have {
		g.readURL(cmd, c, v)
		v.recent = nil // already printed line by line
		err = p.text("\n" + v.frame(time.Now(), 0, 0))
	}
	return err
}

// printLog prints a log event and skips every other message.
func (g *globals) printLog(p *printer, m client.DeploymentMessage) error {
	if m.Event == nil || m.Event.Type != apiv1.DeploymentEventTypeLog {
		return nil
	}
	if g.json {
		return p.jsonLine(deployJSON(m))
	}
	return p.text(deployEventLine(m.Event) + "\n")
}

// followLogs prints log lines until the deployment ends.
func (g *globals) followLogs(cmd *cobra.Command, c *client.Client, id string) error {
	p := g.out()
	var last apiv1.Deployment
	err := c.WatchDeployment(cmd.Context(), id, func(m client.DeploymentMessage) error {
		if m.Snapshot != nil {
			last = *m.Snapshot
		}
		return g.printLog(p, m)
	})
	return deployEnd(cmd.Context(), err, &last, id, "charlie deploy logs "+id+" --follow")
}

// storedLogs prints the log lines recorded so far. A finished deployment's
// stream closes by itself; for one that is still going it stops once the
// stored events are in (logsQuiet, logsMax).
func (g *globals) storedLogs(cmd *cobra.Command, c *client.Client, id string) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	p := g.out()
	var (
		snap     *apiv1.Deployment
		quiet    *time.Timer
		deadline time.Time
	)
	err := c.StreamDeploymentEvents(ctx, id, 0, func(m client.DeploymentMessage) error {
		switch {
		case snap == nil && m.Snapshot != nil:
			snap = m.Snapshot
			if !deployFinished(snap.Status) {
				quiet, deadline = time.AfterFunc(logsQuiet, cancel), time.Now().Add(logsMax)
			}
		case quiet != nil && time.Now().Before(deadline):
			quiet.Reset(logsQuiet)
		}
		return g.printLog(p, m)
	})
	if quiet != nil {
		quiet.Stop()
	}
	switch {
	case cmd.Context().Err() != nil:
		return cmd.Context().Err()
	case err != nil && ctx.Err() == nil: // not our own stop
		return err
	}
	if snap != nil && !deployFinished(snap.Status) {
		fmt.Fprintf(g.app.Stderr, "deployment %s is %s; follow it with: charlie deploy logs %s --follow\n", id, snap.Status, id)
	}
	return nil
}
