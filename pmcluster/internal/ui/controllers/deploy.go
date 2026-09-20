package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

// Deploy exposes the deploy-a-stack form and its submit handler.
type Deploy struct{ *Controller }

type deployData struct {
	Form   deployForm
	Result *deployResult
	Error  string
	Msg    string
}

type deployForm struct {
	AppName  string
	Version  string
	RepoURL  string
	File     string
	Manifest string
}

type deployResult struct {
	Stack    string
	Revision int64
}

// Page renders the deploy form (with any previously submitted values).
func (c Deploy) Page(g *gin.Context) {
	c.Views.Fragment(g, "deploy", deployData{Form: deployForm{
		AppName: g.DefaultQuery("app", ""),
		Version: g.DefaultQuery("version", ""),
		RepoURL: g.DefaultQuery("repo", ""),
		File:    g.DefaultQuery("file", ""),
	}})
}

// Submit posts a manifest to the daemon's deploy endpoint.
func (c Deploy) Submit(g *gin.Context) {
	ctx := g.Request.Context()
	f := deployForm{
		AppName:  g.PostForm("app_name"),
		Version:  g.PostForm("version"),
		RepoURL:  g.PostForm("repo_url"),
		File:     g.PostForm("file"),
		Manifest: g.PostForm("manifest"),
	}
	d := deployData{Form: f}

	_, _, configured := c.loadParams(ctx)
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "deploy", d)
		return
	}
	if f.Manifest == "" {
		d.Error = "manifest is required."
		c.Views.Fragment(g, "deploy", d)
		return
	}

	res, err := c.API.Deploy(ctx, pmapi.DeployPayload{
		AppName:  f.AppName,
		Version:  f.Version,
		RepoURL:  f.RepoURL,
		File:     f.File,
		Manifest: f.Manifest,
	})
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "deploy", d)
		return
	}
	d.Result = &deployResult{Stack: res.Stack, Revision: res.Revision}
	d.Msg = "Deployment accepted."
	c.Views.Fragment(g, "deploy", d)
}
