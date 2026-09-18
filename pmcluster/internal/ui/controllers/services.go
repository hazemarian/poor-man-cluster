package controllers

import (
	"fmt"

	"github.com/gin-gonic/gin"
)

// Services lists swarm services and exposes whitelisted per-service ops
// (tasks, logs, restart, exec). This replaces the Portainer surfaces for
// day-to-day operator work.
type Services struct{ *Controller }

type servicesData struct {
	Services []serviceRow
	Count    int
	Error    string
	Msg      string
}

type serviceRow struct {
	Name        string // full swarm service name (stack_service)
	ServiceName string // unqualified name (service), for URL params
	Stack       string
	Replicas    uint64
	Desired     uint64
	Image       string
	Mode        string
	Updated     int64
}

// List renders the services table (optionally scoped to one stack).
func (c Services) List(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := servicesData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "services", d)
		return
	}
	svcs, err := c.API.ListServices(ctx, "")
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "services", d)
		return
	}
	for _, s := range svcs {
		d.Services = append(d.Services, serviceRow{
			Name: s.Name, Stack: s.Stack, Replicas: s.Replicas, Desired: s.Desired,
			Image: s.Image, Mode: s.Mode, Updated: s.Updated,
			ServiceName: unqualifiedServiceName(s.Name, s.Stack),
		})
	}
	d.Count = len(d.Services)
	c.Views.Fragment(g, "services", d)
}

type serviceDetailData struct {
	Stack   string
	Service string
	Tasks   []serviceTaskRow
	Logs    []serviceLogRow
	Tab     string // "tasks" | "logs"
	Error   string
	Msg     string
}

type serviceTaskRow struct {
	TaskID string
	Node   string
	Slot   int64
	State  string
	Error  string
	Start  int64
}

type serviceLogRow struct {
	Stream string
	Line   string
}

// Tasks renders one service's task (crash/restart) history.
func (c Services) Tasks(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{Stack: stack, Service: service, Tab: "tasks"}
	rows, err := c.API.ServiceTasks(ctx, stack, service)
	if err != nil {
		d.Error = err.Error()
	} else {
		for _, t := range rows {
			d.Tasks = append(d.Tasks, serviceTaskRow{
				TaskID: t.TaskID, Node: t.Node, Slot: t.Slot,
				State: t.State, Error: t.Error, Start: t.StartedAt,
			})
		}
	}
	c.Views.Fragment(g, "servicedetail", d)
}

// Logs tails one service's stdout/stderr.
func (c Services) Logs(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{Stack: stack, Service: service, Tab: "logs"}
	lines, err := c.API.ServiceLogs(ctx, stack, service, 200)
	if err != nil {
		d.Error = err.Error()
	} else {
		for _, ln := range lines {
			d.Logs = append(d.Logs, serviceLogRow{Stream: ln.Stream, Line: ln.Line})
		}
	}
	c.Views.Fragment(g, "servicedetail", d)
}

// Restart forces a rolling restart, then re-renders the service's tasks.
func (c Services) Restart(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{Stack: stack, Service: service, Tab: "tasks", Msg: fmt.Sprintf("Restart triggered for %s_%s.", stack, service)}
	if err := c.API.RestartService(ctx, stack, service); err != nil {
		d.Msg = ""
		d.Error = err.Error()
	}
	rows, err := c.API.ServiceTasks(ctx, stack, service)
	if err != nil {
		if d.Error == "" {
			d.Error = err.Error()
		}
		c.Views.Fragment(g, "servicedetail", d)
		return
	}
	for _, t := range rows {
		d.Tasks = append(d.Tasks, serviceTaskRow{
			TaskID: t.TaskID, Node: t.Node, Slot: t.Slot,
			State: t.State, Error: t.Error, Start: t.StartedAt,
		})
	}
	c.Views.Fragment(g, "servicedetail", d)
}

// Exec runs a non-interactive command and shows the output inline.
func (c Services) Exec(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("stack"), g.Param("service")
	c.loadParams(ctx)
	d := serviceDetailData{Stack: stack, Service: service, Tab: "tasks"}
	argv := splitArgs(g.PostForm("argv"))
	if len(argv) == 0 {
		d.Error = "argv: required (e.g. whoami, or sh -c \"cat /etc/hosts\")"
		c.Views.Fragment(g, "servicedetail", d)
		return
	}
	res, err := c.API.ExecService(ctx, stack, service, argv)
	if err != nil {
		d.Error = err.Error()
	} else {
		d.Msg = fmt.Sprintf("exit %d", res.ExitCode)
		if res.Stdout != "" {
			d.Logs = append(d.Logs, serviceLogRow{Stream: "stdout", Line: res.Stdout})
		}
		if res.Stderr != "" {
			d.Logs = append(d.Logs, serviceLogRow{Stream: "stderr", Line: res.Stderr})
		}
		d.Tab = "logs"
	}
	c.Views.Fragment(g, "servicedetail", d)
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
