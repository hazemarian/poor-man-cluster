package remote

import (
	"context"
	"net/http"
	"net/url"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks"
)

// Stacks is the HTTP adapter for stacks.Reader.
type Stacks struct{ c *Client }

// NewStacks builds the remote stacks adapter.
func NewStacks(c *Client) stacks.Reader { return &Stacks{c: c} }

type stackDTO struct {
	Name            string `json:"name"`
	CurrentRevision int64  `json:"current_revision"`
	RepoURL         string `json:"repo_url"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

type stackListDTO struct {
	Stacks []stackDTO `json:"stacks"`
}

type revisionMetaDTO struct {
	Revision  int64 `json:"revision"`
	CreatedAt int64 `json:"created_at"`
}

type stackDetailDTO struct {
	Stack     stackDTO          `json:"stack"`
	Revisions []revisionMetaDTO `json:"revisions"`
}

func (a *Stacks) List(ctx context.Context) ([]stacks.Stack, error) {
	var out stackListDTO
	if err := a.c.do(ctx, http.MethodGet, "/stacks", nil, &out); err != nil {
		return nil, err
	}
	ss := make([]stacks.Stack, 0, len(out.Stacks))
	for _, d := range out.Stacks {
		ss = append(ss, d.stack())
	}
	return ss, nil
}

func (a *Stacks) Get(ctx context.Context, name string) (*stacks.Stack, error) {
	var out stackDetailDTO
	if err := a.c.do(ctx, http.MethodGet, "/stacks/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	s := out.Stack.stack()
	return &s, nil
}

func (a *Stacks) Revisions(ctx context.Context, name string, limit int) ([]stacks.Revision, error) {
	var out stackDetailDTO
	if err := a.c.do(ctx, http.MethodGet, "/stacks/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	revs := make([]stacks.Revision, 0, len(out.Revisions))
	for _, m := range out.Revisions {
		revs = append(revs, stacks.Revision{
			StackName: name,
			Revision:  m.Revision,
			CreatedAt: m.CreatedAt,
		})
	}
	return revs, nil
}

func (d stackDTO) stack() stacks.Stack {
	return stacks.Stack{
		Name:            d.Name,
		CurrentRevision: d.CurrentRevision,
		RepoURL:         d.RepoURL,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
	}
}

// Deploy is the HTTP adapter for stacks.Deployer.
type Deploy struct{ c *Client }

// NewDeploy builds the remote deploy adapter.
func NewDeploy(c *Client) stacks.Deployer { return &Deploy{c: c} }

type deployResultDTO struct {
	Stack        string `json:"stack"`
	Revision     int64  `json:"revision"`
	NewRevision  int64  `json:"new_revision"`
	RolledBackTo int64  `json:"rolled_back_to"`
}

func (a *Deploy) Deploy(ctx context.Context, p stacks.Payload) (*stacks.Result, error) {
	var out deployResultDTO
	if err := a.c.do(ctx, http.MethodPost, "/stacks", map[string]string{
		"app_name": p.AppName,
		"repo_url": p.RepoURL,
		"version":  p.Version,
		"manifest": p.Manifest,
	}, &out); err != nil {
		return nil, err
	}
	return &stacks.Result{StackName: out.Stack, Revision: out.Revision}, nil
}

func (a *Deploy) Rollback(ctx context.Context, stackName string, sourceRevision int64) (*stacks.Result, error) {
	var out deployResultDTO
	if err := a.c.do(ctx, http.MethodPost, "/stacks/"+url.PathEscape(stackName)+"/rollback", map[string]int64{
		"revision": sourceRevision,
	}, &out); err != nil {
		return nil, err
	}
	rev := out.Revision
	if out.NewRevision != 0 {
		rev = out.NewRevision
	}
	return &stacks.Result{StackName: out.Stack, Revision: rev}, nil
}

func (a *Deploy) Sync(ctx context.Context, stackName string) (*stacks.Result, error) {
	var out deployResultDTO
	if err := a.c.do(ctx, http.MethodPost, "/stacks/"+url.PathEscape(stackName)+"/sync", nil, &out); err != nil {
		return nil, err
	}
	return &stacks.Result{StackName: out.Stack, Revision: out.Revision}, nil
}

func (a *Deploy) Undeploy(ctx context.Context, stackName string) error {
	return a.c.do(ctx, http.MethodDelete, "/stacks/"+url.PathEscape(stackName), nil, nil)
}
