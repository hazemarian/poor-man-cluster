package remote

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
)

// Services is the HTTP adapter for the whitelisted service ops.
type Services struct{ c *Client }

// NewServices builds the remote services adapter.
func NewServices(c *Client) services.Service { return &Services{c: c} }

type serviceDTO struct {
	Name     string `json:"name"`
	Stack    string `json:"stack"`
	Replicas uint64 `json:"replicas"`
	Desired  uint64 `json:"desired"`
	Image    string `json:"image"`
	Mode     string `json:"mode"`
	Updated  int64  `json:"updated"`
}

type serviceListDTO struct {
	Services []serviceDTO `json:"services"`
}

type taskDTO struct {
	TaskID     string `json:"task_id"`
	Node       string `json:"node"`
	Slot       int64  `json:"slot"`
	State      string `json:"state"`
	Error      string `json:"error"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

type tasksDTO struct {
	Tasks []taskDTO `json:"tasks"`
}

type logLineDTO struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

type logsDTO struct {
	Logs []logLineDTO `json:"logs"`
}

type execResultDTO struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (a *Services) List(ctx context.Context, stack string) ([]services.ServiceSummary, error) {
	path := "/services"
	if stack != "" {
		path += "/" + url.PathEscape(stack)
	}
	var out serviceListDTO
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	ss := make([]services.ServiceSummary, 0, len(out.Services))
	for _, d := range out.Services {
		ss = append(ss, d.summary())
	}
	return ss, nil
}

func (a *Services) Tasks(ctx context.Context, stack, service string) ([]services.TaskRun, error) {
	var out tasksDTO
	if err := a.c.do(ctx, http.MethodGet, svcPath(stack, service)+"/tasks", nil, &out); err != nil {
		return nil, err
	}
	ts := make([]services.TaskRun, 0, len(out.Tasks))
	for _, d := range out.Tasks {
		ts = append(ts, services.TaskRun{
			TaskID:     d.TaskID,
			Node:       d.Node,
			Slot:       d.Slot,
			State:      d.State,
			Error:      d.Error,
			StartedAt:  d.StartedAt,
			FinishedAt: d.FinishedAt,
		})
	}
	return ts, nil
}

func (a *Services) Logs(ctx context.Context, stack, service string, tail int) ([]services.LogLine, error) {
	var out logsDTO
	if err := a.c.do(ctx, http.MethodGet, svcPath(stack, service)+"/logs?tail="+itoa(tail), nil, &out); err != nil {
		return nil, err
	}
	ls := make([]services.LogLine, 0, len(out.Logs))
	for _, d := range out.Logs {
		ls = append(ls, services.LogLine{Stream: d.Stream, Line: d.Line})
	}
	return ls, nil
}

func (a *Services) Restart(ctx context.Context, stack, service string) error {
	return a.c.do(ctx, http.MethodPost, svcPath(stack, service)+"/restart", nil, nil)
}

func (a *Services) Exec(ctx context.Context, stack, service string, argv []string) (*services.ExecResult, error) {
	var out execResultDTO
	if err := a.c.do(ctx, http.MethodPost, svcPath(stack, service)+"/exec", map[string]any{"argv": argv}, &out); err != nil {
		return nil, err
	}
	return &services.ExecResult{
		ExitCode: out.ExitCode,
		Stdout:   out.Stdout,
		Stderr:   out.Stderr,
	}, nil
}

func (d serviceDTO) summary() services.ServiceSummary {
	return services.ServiceSummary{
		Name:     d.Name,
		Stack:    d.Stack,
		Replicas: d.Replicas,
		Desired:  d.Desired,
		Image:    d.Image,
		Mode:     d.Mode,
		Updated:  d.Updated,
	}
}

func svcPath(stack, service string) string {
	return "/services/" + url.PathEscape(stack) + "/" + url.PathEscape(service)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
