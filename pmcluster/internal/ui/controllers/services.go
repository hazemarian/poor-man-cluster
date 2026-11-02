package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// ---------------------------------------------------------------------------
// Shared error plumbing for the runtime pages (Services, TLS, Backups).
//
// The v2 fragments never print a raw upstream error inline: a banner carries a
// humanised sentence (looked up from a dictionary key) and the untouched
// payload goes into a <details> disclosure underneath it. humanErr returns "no
// key" for a nil error, so a page can never render a banner by accident.
// ---------------------------------------------------------------------------

// errKeyNotConfigured is the banner key used when the API URL/token are unset.
const errKeyNotConfigured = "err.api_not_configured"

// humanErr maps a request failure to (dictionary key, raw upstream payload).
func humanErr(key string, err error) (string, string) {
	if err == nil {
		return "", ""
	}
	if key == "" {
		key = "err.upstream"
	}
	var pe *pmapi.Error
	if errors.As(err, &pe) {
		body := strings.TrimSpace(pe.Body)
		if body == "" {
			body = err.Error()
		}
		return key, fmt.Sprintf("HTTP %d %s\n%s", pe.Status, http.StatusText(pe.Status), body)
	}
	return key, err.Error()
}

// Services lists swarm services and exposes the per-service ops an operator
// needs (tasks, logs, restart, exec) — the Portainer surfaces for day-to-day
// work.
type Services struct{ *Controller }

type servicesData struct {
	// Services are the CUSTOMER app services; Platform are the platform-managed
	// services (edge/traefik/observability/backup/sso), kept apart so an operator
	// reading the app surface never mistakes platform plumbing for their own apps.
	Services []serviceRow
	Platform []serviceRow
	// Known separates "the list was read and is empty" from "the read failed":
	// the second one is never rendered as zero services.
	Known  bool
	Count  int64
	Ready  int64 // every desired replica is up
	Short  int64 // running fewer tasks than desired
	Stacks int64 // distinct stacks owning at least one service

	ErrKey string
	ErrRaw string
	MsgKey string
	MsgA   string
	MsgB   string
}

type serviceRow struct {
	Name         string // full swarm service name (stack_service)
	ServiceName  string // unqualified name (service), for URL params
	Stack        string
	Replicas     int64
	Desired      int64
	Image        string
	Mode         string
	Updated      int64
	Converged    bool   // desired > 0 and every replica is up
	Short        bool   // running fewer tasks than desired
	Paused       bool   // desired == 0
	Complete     bool   // one-shot job that already finished (0 running of 1 desired)
	ImageAge     int64  // days since the local image was built; 0 when unknown or < 1 day
	PausedUpdate bool   // swarm paused this service's update after a task failure
	UpdateError  string // the orchestrator's reason for pausing
	Routable     bool   // belongs to a stack, so the per-service routes resolve
	Node         string // io.pmcluster.node label: node this service is pinned to ("" when unconstrained)
	RunningNode  string // node the running task is on ("" when none is running)
	// TaskError is why the service has no running replica: the error from its
	// newest rejected/failed swarm task, e.g. "failed to resolve reference
	// ghcr.io/nextrum-sy/adam-world-dev:dev-v… not found".
	//
	// It is not the same thing as UpdateError, and that difference cost real time:
	// a deploy can be accepted and recorded while every task is then rejected by
	// the runtime. The stack page showed 0/1 with no reason, and the answer existed
	// only behind the per-service Tasks tab.
	TaskError string
	// TaskErrorShort is TaskError clipped for the table cell; the full text stays
	// in TaskError for the title tooltip.
	TaskErrorShort string
}

// List renders the services table.
func (c Services) List(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := servicesData{}
	if !configured {
		d.ErrKey = errKeyNotConfigured
		c.Views.Fragment(g, "services", d)
		return
	}
	svcs, err := c.API.ListServices(ctx, "")
	if err != nil {
		d.ErrKey, d.ErrRaw = humanErr("err.services", err)
		c.Views.Fragment(g, "services", d)
		return
	}
	d.Known = true
	stacks := map[string]bool{}
	for _, s := range svcs {
		row := newServiceRow(s)
		if s.Platform {
			d.Platform = append(d.Platform, row)
			continue
		}
		if row.Converged {
			d.Ready++
		}
		if row.Short {
			d.Short++
		}
		if s.Stack != "" {
			stacks[s.Stack] = true
		}
		d.Services = append(d.Services, row)
	}
	d.Count = int64(len(d.Services))
	d.Stacks = int64(len(stacks))
	d.Services = c.attachTaskErrors(ctx, d.Services)
	d.Platform = c.attachTaskErrors(ctx, d.Platform)
	c.Views.Fragment(g, "services", d)
}

// attachTaskErrors fills TaskError for the services that are not converged.
//
// It asks the daemon per failing service rather than for the whole stack, and it
// stops after taskErrorLookups: a page that is healthy makes zero extra calls, and
// a page where everything is down cannot turn into a burst of them.
func (c *Controller) attachTaskErrors(ctx context.Context, rows []serviceRow) []serviceRow {
	const taskErrorLookups = 5
	lookups := 0
	for i := range rows {
		if lookups >= taskErrorLookups {
			break
		}
		r := &rows[i]
		if !r.Short || r.Complete || r.Stack == "" || r.ServiceName == "" {
			continue
		}
		lookups++
		tasks, err := c.API.ServiceTasks(ctx, r.Stack, r.ServiceName)
		if err != nil {
			continue
		}
		// Newest first: the task list is returned in swarm order, so take the most
		// recent non-empty error and let a running task win outright.
		for _, t := range tasks {
			if t.State == "running" {
				r.TaskError = ""
				break
			}
			if msg := strings.TrimSpace(t.Error); msg != "" {
				r.TaskError = msg
				r.TaskErrorShort = clip(msg, 110)
				break
			}
		}
	}
	return rows
}

// clip shortens a one-line message for a table cell, on a word boundary when one
// is near the cut.
func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// newServiceRow projects a pmapi.Service into the render-ready serviceRow used
// by the services, platform and stack pages. Kept in one place so a column
// change never diverges between surfaces.
func newServiceRow(s pmapi.Service) serviceRow {
	return serviceRow{
		Name:         s.Name,
		ServiceName:  unqualifiedServiceName(s.Name, s.Stack),
		Stack:        s.Stack,
		Replicas:     int64(s.Replicas),
		Desired:      int64(s.Desired),
		Image:        s.Image,
		Mode:         s.Mode,
		Updated:      s.Updated,
		Converged:    s.Desired > 0 && s.Replicas >= s.Desired,
		Short:        s.Replicas < s.Desired,
		Paused:       s.Desired == 0,
		Complete:     s.RunOnce && s.Desired > 0 && s.Replicas == 0,
		ImageAge:     imageAgeDays(s.ImageCreated),
		PausedUpdate: s.UpdateState == "paused" && !completedRunOnce(s.RunOnce, s.Desired, s.Replicas),
		UpdateError:  s.UpdateError,
		Routable:     s.Stack != "",
		Node:         s.Node,
		RunningNode:  s.RunningNode,
	}
}

type serviceDetailData struct {
	Stack    string
	Service  string
	FullName string
	Tab      string // "tasks" | "logs"

	Svc      *serviceMeta
	SvcKnown bool

	Tasks      []serviceTaskRow
	TasksKnown bool
	Logs       []serviceLogRow
	LogsKnown  bool
	TailCount  string // tail window, for the logs foot note

	ExecRan   bool
	ExecExit  int64
	ExecArgv  string
	ExecEmpty bool
	// LogText is the joined log/exec output, for the copy button; empty means
	// there is nothing to copy and the button is not rendered.
	LogText string

	ErrKey string
	ErrRaw string
	MsgKey string
	MsgA   string
	MsgB   string
}

// serviceMeta is the replica/image card shown above the tabs.
type serviceMeta struct {
	Name         string
	Image        string
	Mode         string
	Updated      int64
	Replicas     int64
	Desired      int64
	Converged    bool
	Short        bool
	Paused       bool
	Complete     bool
	ImageAge     int64
	PausedUpdate bool
	UpdateError  string
	Node         string // node the service is pinned to ("" when unconstrained)
	RunningNode  string // node the running task is on ("" when none is running)
	// TaskError is why the service has no running replica: the error from its
	// newest rejected/failed swarm task, e.g. "failed to resolve reference
	// ghcr.io/nextrum-sy/adam-world-dev:dev-v… not found".
	//
	// It is not the same thing as UpdateError, and that difference cost real time:
	// a deploy can be accepted and recorded while every task is then rejected by
	// the runtime. The stack page showed 0/1 with no reason, and the answer existed
	// only behind the per-service Tasks tab.
	TaskError string
	// TaskErrorShort is TaskError clipped for the table cell; the full text stays
	// in TaskError for the title tooltip.
	TaskErrorShort string
}

// completedRunOnce reports whether a service is a one-shot job that already
// finished (0 running of 1 desired). Such jobs are completed, never degraded
// or paused.
func completedRunOnce(runOnce bool, desired, replicas uint64) bool {
	return runOnce && desired > 0 && replicas == 0
}

// imageAgeDays returns whole days since a locally-cached image was built
// (0 when the image is not cached or was built today). A large value means
// the node is running a stale cached image — usually a registry-auth gap.
func imageAgeDays(created int64) int64 {
	if created <= 0 {
		return 0
	}
	days := (time.Now().Unix() - created) / 86400
	if days < 0 {
		return 0
	}
	return days
}

type serviceTaskRow struct {
	TaskID    string
	Node      string
	Slot      int64
	RawState  string // the daemon's own word, unmapped
	State     string // dictionary key (st.*), empty when unmapped
	Pill      string // good | warn | bad | info | plain
	TaskError string
	Started   int64
	Finished  int64
}

type serviceLogRow struct {
	Stream    string // raw stream name from the daemon
	StreamKey string // dictionary key, empty when unmapped
	Line      string
	IsErr     bool
}

// taskState maps the daemon's task-state vocabulary onto the shared status
// words. An unmapped state keeps its raw word so the console never invents a
// state it does not know.
func taskState(raw string) (key, pill string) {
	switch strings.ToLower(raw) {
	case "running":
		return "st.running", "good"
	case "ready":
		return "st.ready", "good"
	case "complete", "completed", "shutdown":
		return "st.done", "plain"
	case "failed", "rejected", "orphaned":
		return "st.failed", "bad"
	case "pending", "new", "allocated", "assigned", "accepted", "preparing", "starting":
		return "st.pending", "info"
	case "draining", "remove":
		return "st.draining", "warn"
	}
	return "", "plain"
}

// logStream names the stream in the reader's language, falling back to the
// daemon's own word.
func logStream(raw string) (key string, isErr bool) {
	switch strings.ToLower(raw) {
	case "stdout":
		return "services.exec_stream_stdout", false
	case "stderr":
		return "services.exec_stream_stderr", true
	}
	return "", false
}

// meta reads the service's own card (replicas/image) from the service list.
// A failure here is not a page error: the tabs below still work, so the card
// renders as unknown instead of showing zeros.
func (c Services) meta(ctx *gin.Context, stack, service string) (*serviceMeta, bool) {
	svcs, err := c.API.ListServices(ctx.Request.Context(), stack)
	if err != nil {
		return nil, false
	}
	want := stack + "_" + service
	for _, s := range svcs {
		if s.Name != want && s.Name != service {
			continue
		}
		return &serviceMeta{
			Name: s.Name, Image: s.Image, Mode: s.Mode, Updated: s.Updated,
			Replicas: int64(s.Replicas), Desired: int64(s.Desired),
			Converged:    s.Desired > 0 && s.Replicas >= s.Desired,
			Short:        s.Replicas < s.Desired,
			Paused:       s.Desired == 0,
			Complete:     s.RunOnce && s.Desired > 0 && s.Replicas == 0,
			ImageAge:     imageAgeDays(s.ImageCreated),
			PausedUpdate: s.UpdateState == "paused" && !completedRunOnce(s.RunOnce, s.Desired, s.Replicas),
			UpdateError:  s.UpdateError,
			Node:         s.Node,
			RunningNode:  s.RunningNode,
		}, true
	}
	return nil, false
}

// tasks loads the task history into d.
func (c Services) tasks(ctx *gin.Context, stack, service string, d *serviceDetailData) {
	rows, err := c.API.ServiceTasks(ctx.Request.Context(), stack, service)
	if err != nil {
		d.TasksKnown = false
		if d.ErrKey == "" {
			d.ErrKey, d.ErrRaw = humanErr("err.service_tasks", err)
		}
		return
	}
	d.TasksKnown = true
	for _, t := range rows {
		key, pill := taskState(t.State)
		d.Tasks = append(d.Tasks, serviceTaskRow{
			TaskID: t.TaskID, Node: t.Node, Slot: t.Slot,
			RawState: t.State, State: key, Pill: pill,
			TaskError: t.Error, Started: t.StartedAt, Finished: t.FinishedAt,
		})
	}
}

// Tasks renders one service's task (crash/restart) history.
func (c Services) Tasks(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{
		Stack: stack, Service: service, FullName: stack + "_" + service,
		Tab: "tasks", TailCount: "200",
	}
	d.Svc, d.SvcKnown = c.meta(g, stack, service)
	c.tasks(g, stack, service, &d)
	c.Views.Fragment(g, "servicedetail", d)
}

// Logs tails one service's stdout/stderr.
func (c Services) Logs(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{
		Stack: stack, Service: service, FullName: stack + "_" + service,
		Tab: "logs", TailCount: "200",
	}
	d.Svc, d.SvcKnown = c.meta(g, stack, service)
	lines, err := c.API.ServiceLogs(ctx, stack, service, 200)
	if err != nil {
		d.ErrKey, d.ErrRaw = humanErr("err.service_logs", err)
	} else {
		d.LogsKnown = true
		for _, ln := range lines {
			key, isErr := logStream(ln.Stream)
			d.Logs = append(d.Logs, serviceLogRow{
				Stream: ln.Stream, StreamKey: key, Line: ln.Line, IsErr: isErr,
			})
		}
	}
	d.LogText = logText(d.Logs)
	// A polling request (the panel's hx-trigger) asks for the log rows alone,
	// so the swap never re-embeds the whole detail page into the panel.
	if g.Query("fragment") == "logs" {
		c.Views.Fragment(g, "servicelogs", d)
		return
	}
	c.Views.Fragment(g, "servicedetail", d)
}

// Restart forces a rolling restart, then re-renders the service's tasks.
func (c Services) Restart(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{
		Stack: stack, Service: service, FullName: stack + "_" + service,
		Tab: "tasks", TailCount: "200",
	}
	d.Svc, d.SvcKnown = c.meta(g, stack, service)
	if err := c.API.RestartService(ctx, stack, service); err != nil {
		d.ErrKey, d.ErrRaw = humanErr("err.service_restart", err)
	} else {
		d.MsgKey, d.MsgA = "services.restart_msg", d.FullName
	}
	c.tasks(g, stack, service, &d)
	c.Views.Fragment(g, "servicedetail", d)
}

// Exec runs a non-interactive command and shows the output inline.
func (c Services) Exec(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{
		Stack: stack, Service: service, FullName: stack + "_" + service,
		Tab: "tasks", TailCount: "200",
	}
	d.Svc, d.SvcKnown = c.meta(g, stack, service)
	argv := splitArgs(g.PostForm("argv"))
	d.ExecArgv = strings.Join(argv, " ")
	if len(argv) == 0 {
		// Client-side validation, not an upstream failure: no raw payload.
		d.ErrKey = "services.exec_required"
		c.tasks(g, stack, service, &d)
		c.Views.Fragment(g, "servicedetail", d)
		return
	}
	res, err := c.API.ExecService(ctx, stack, service, argv)
	if err != nil {
		d.ErrKey, d.ErrRaw = humanErr("err.service_exec", err)
		c.tasks(g, stack, service, &d)
		c.Views.Fragment(g, "servicedetail", d)
		return
	}
	d.ExecRan = true
	d.ExecExit = int64(res.ExitCode)
	d.MsgKey, d.MsgA = "services.exec_exit", strconv.Itoa(res.ExitCode)
	d.Tab = "logs"
	d.LogsKnown = true
	if res.Stdout != "" {
		d.Logs = append(d.Logs, serviceLogRow{Stream: "stdout", StreamKey: "services.exec_stream_stdout", Line: res.Stdout})
	}
	if res.Stderr != "" {
		d.Logs = append(d.Logs, serviceLogRow{Stream: "stderr", StreamKey: "services.exec_stream_stderr", Line: res.Stderr, IsErr: true})
	}
	if res.Stdout == "" && res.Stderr == "" {
		d.ExecEmpty = true
	}
	d.LogText = logText(d.Logs)
	c.Views.Fragment(g, "servicedetail", d)
}

// logText joins rendered log lines for the copy button.
func logText(rows []serviceLogRow) string {
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(r.Line)
	}
	return b.String()
}

// splitArgs splits a shell-like string on whitespace, respecting double quotes.
func splitArgs(s string) []string {
	var args []string
	var cur []rune
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			if len(cur) > 0 {
				args = append(args, string(cur))
				cur = nil
			}
		default:
			cur = append(cur, r)
		}
	}
	if len(cur) > 0 {
		args = append(args, string(cur))
	}
	return args
}

// unqualifiedServiceName strips the "<stack>_" prefix from a full swarm
// service name, so UI routes can pass the unqualified name to the daemon
// (which re-resolves it with the stack namespace guard). Services not
// belonging to a stack keep their full name.
func unqualifiedServiceName(full, stack string) string {
	if stack != "" {
		if p := stack + "_"; len(full) > len(p) && full[:len(p)] == p {
			return full[len(p):]
		}
	}
	return full
}
