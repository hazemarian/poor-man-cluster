package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/middleware"
)

// Overview renders the app shell and the cluster overview fragment.
type Overview struct{ *Controller }

// App renders the HTMX application shell (sidebar + empty #view). All data
// loads via fragment requests from the shell itself.
func (c Overview) App(g *gin.Context) {
	c.Views.Page(g, "app", gin.H{
		"User":          middleware.CurrentUser(g),
		"Version":       c.Version,
		"LoginDisabled": c.Auth.LoginDisabled,
	})
}

type overviewData struct {
	Me    *meModel
	Info  *infoModel
	Nodes []nodeModel
	Error string
	Msg   string
}

// meModel/infoModel/nodeModel are thin view aliases so templates don't import
// the pmapi package.
type meModel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Fragment renders cluster info + nodes, or an error if not configured.
func (c Overview) Fragment(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)

	d := overviewData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings to set the API URL and token."
		c.Views.Fragment(g, "overview", d)
		return
	}

	me, err := c.API.Me(ctx)
	if err == nil {
		d.Me = &meModel{ID: me.ID, Name: me.Name}
	}
	info, errI := c.API.ClusterInfo(ctx)
	if errI == nil {
		d.Info = &infoModel{
			NodeName: info.NodeName, ServerVersion: info.ServerVersion,
			OS: info.OS, Arch: info.Arch, CPUs: info.CPUs, MemoryBytes: info.MemoryBytes,
			SwarmState: info.Swarm.State, SwarmManagers: info.Swarm.Managers,
			SwarmNodes: info.Swarm.Nodes, ControlAvailable: info.Swarm.ControlAvailable,
		}
	}
	nodes, errN := c.API.Nodes(ctx)
	if errN == nil {
		for _, n := range nodes {
			d.Nodes = append(d.Nodes, nodeModel{
				Hostname: n.Hostname, Role: n.Role, Status: n.Status,
				IsLeader: n.IsLeader, Availability: n.Availability, EngineVersion: n.EngineVersion,
			})
		}
	}

	switch {
	case err != nil:
		d.Error = err.Error()
	case errI != nil:
		d.Error = errI.Error()
	case errN != nil:
		d.Error = errN.Error()
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
