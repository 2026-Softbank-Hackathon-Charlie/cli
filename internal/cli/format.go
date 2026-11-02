package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// printer writes to stdout.
type printer struct {
	w     io.Writer
	color bool
}

func (p *printer) json(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(p.w, "%s\n", b)
	return err
}

func (p *printer) jsonLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(p.w, "%s\n", b)
	return err
}

func (p *printer) text(s string) error {
	_, err := io.WriteString(p.w, p.paint(s))
	return err
}

// paint colors status markers. It runs after tabwriter has aligned the
// plain text, so escape codes never skew column widths.
func (p *printer) paint(s string) string {
	if !p.color {
		return s
	}
	return markerColors.Replace(s)
}

var markerColors = strings.NewReplacer(
	"[ok]", "\x1b[32m[ok]\x1b[0m",
	"[x]", "\x1b[31m[x]\x1b[0m",
	"[..]", "\x1b[33m[..]\x1b[0m",
	"[ ]", "\x1b[2m[ ]\x1b[0m",
)

// table aligns rows; the first row is the header. Empty cells print as "-".
func table(rows [][]string) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = cell(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
	return b.String()
}

// fields aligns "Key:  value" lines.
func fields(pairs [][2]string) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, p := range pairs {
		fmt.Fprintf(tw, "%s:\t%s\n", p[0], cell(p[1]))
	}
	tw.Flush()
	return b.String()
}

// cell keeps one value on one line and inside its column.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "-"
	}
	return s
}

// clock is a local time of day, for event lines.
func clock(t time.Time) string { return t.Local().Format("15:04:05") }

// stamp is a local date and time.
func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

func stampPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return stamp(*t)
}

// human renders a duration the way people say it: 850ms, 12.3s, 4m05s, 2h03m.
func human(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

func millis(p *int64) string {
	if p == nil {
		return ""
	}
	return human(time.Duration(*p) * time.Millisecond)
}

func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func count(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func percent(f float32) string { return fmt.Sprintf("%.2f%%", f*100) }

func runMark(s apiv1.RunStatus) string {
	switch s {
	case apiv1.RunStatusSucceeded:
		return "[ok]"
	case apiv1.RunStatusRunning, apiv1.RunStatusAwaitingSwitch:
		return "[..]"
	}
	return "[x]"
}

func stepMark(s apiv1.StepStatus) string {
	switch s {
	case apiv1.StepStatusSucceeded:
		return "[ok]"
	case apiv1.StepStatusRunning:
		return "[..]"
	case apiv1.StepStatusFailed:
		return "[x]"
	}
	return "[ ]"
}

func planMark(s apiv1.PlanStatus) string {
	switch s {
	case apiv1.PlanStatusApproved, apiv1.PlanStatusStarted:
		return "[ok]"
	case apiv1.PlanStatusStarting:
		return "[..]"
	case apiv1.PlanStatusRejected:
		return "[x]"
	}
	return "[ ]"
}

func route(src, dst string) string { return src + " -> " + dst }
