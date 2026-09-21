package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Overview renders the app shell and the cluster overview fragment.
type Overview struct{ *Controller }

// App renders the HTMX application shell (sidebar + empty #view). All data
// loads via fragment requests from the shell itself.
//
// The shell's data comes from the renderer's ShellData builder rather than a
// hand-rolled map here: shell chrome (breadcrumb, title, language links, the
// per-request refresh URL) is exactly what that builder exists for, and a
// second copy of it drifts. Passing a map without "Crumb" is how this route
// came to stream "invalid value; expected string" into the document title.
func (c Overview) App(g *gin.Context) {
	if c.Views.ShellData == nil {
		g.Status(http.StatusInternalServerError)
		return
	}
	c.Views.Page(g, "app", c.Views.ShellData(g))
}

type overviewData struct {
	Me    *meModel
	Info  *infoModel
	Nodes []nodeModel

	// The v1 console rendered every failed call as a zero: a broken swarm query
	// looked exactly like an empty cluster. These flags keep "we don't know"
	// separate from "the answer is none", and ErrRaw keeps the payload for the
	// operator who wants it without putting it in the reading path.
	InfoKnown  bool
	NodesKnown bool
	ErrKey     string
	ErrRaw     string
}

// meModel/infoModel/nodeModel are thin view aliases so templates don't import
// the pmapi package.
type meModel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Fragment renders cluster info + nodes, or a humanised failure with the raw
// upstream payload folded away behind a disclosure.
func (c Overview) Fragment(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)

	d := overviewData{}
	if !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "overview", d)
		return
	}

	me, err := c.API.Me(ctx)
	if err == nil {
		d.Me = &meModel{ID: me.ID, Name: me.Name}
	}
	info, errI := c.API.ClusterInfo(ctx)
	if errI == nil {
		d.InfoKnown = true
		d.Info = &infoModel{
			NodeName: info.NodeName, ServerVersion: info.ServerVersion,
			OS: info.OS, Arch: info.Arch, CPUs: info.CPUs, MemoryBytes: info.MemoryBytes,
			SwarmState: info.Swarm.State, SwarmManagers: info.Swarm.Managers,
			SwarmNodes: info.Swarm.Nodes, ControlAvailable: info.Swarm.ControlAvailable,
		}
	}
	nodes, errN := c.API.Nodes(ctx)
	if errN == nil {
		d.NodesKnown = true
		for _, n := range nodes {
			d.Nodes = append(d.Nodes, nodeModel{
				Hostname: n.Hostname, Role: n.Role, Status: n.Status,
				IsLeader: n.IsLeader, Availability: n.Availability, EngineVersion: n.EngineVersion,
			})
		}
	}

	switch {
	case err != nil:
		d.ErrKey, d.ErrRaw = "err.api_unreachable", err.Error()
	case errI != nil:
		d.ErrKey, d.ErrRaw = "err.cluster_info", errI.Error()
	case errN != nil:
		d.ErrKey, d.ErrRaw = "err.nodes", errN.Error()
	}
	c.Views.Fragment(g, "overview", d)
}

type infoModel struct {
	NodeName         string
	ServerVersion    string
	OS               string
	Arch             string
	CPUs             int
	MemoryBytes      int64
	SwarmState       string
	SwarmManagers    int
	SwarmNodes       int
	ControlAvailable bool
}

type nodeModel struct {
	Hostname      string
	Role          string
	Status        string
	IsLeader      bool
	Availability  string
	EngineVersion string
}
