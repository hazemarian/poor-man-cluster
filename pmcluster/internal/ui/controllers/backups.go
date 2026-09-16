package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/ui/pmapi"
)

// Backups lists backups and triggers new ones.
type Backups struct{ *Controller }

type backupsData struct {
	Backups []backupRow
	Name    string
	Error   string
	Msg     string
}

type backupRow struct {
	ID         int64
	Status     string
	StackName  string
	Revision   int64
	StartedAt  int64
	FinishedAt int64
	Archives   int
}

func backupRowFrom(b pmapi.Backup) backupRow {
	return backupRow{
		ID: b.ID, Status: b.Status, StackName: b.StackName, Revision: b.Revision,
		StartedAt: b.StartedAt, FinishedAt: b.FinishedAt, Archives: len(b.ArchivePaths),
	}
}

// List renders the recent backups table.
func (c Backups) List(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := backupsData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "backups", d)
		return
	}
	rows, err := c.API.ListBackups(ctx, 50)
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "backups", d)
		return
	}
	for _, b := range rows {
		d.Backups = append(d.Backups, backupRowFrom(b))
	}
	c.Views.Fragment(g, "backups", d)
}

// Create triggers a backup and re-renders the list.
func (c Backups) Create(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := backupsData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "backups", d)
		return
	}
	if _, err := c.API.CreateBackup(ctx); err != nil {
		d.Error = err.Error()
	} else {
		d.Msg = "Backup triggered."
	}
	rows, err := c.API.ListBackups(ctx, 50)
	if err != nil && d.Error == "" {
		d.Error = err.Error()
	}
	for _, b := range rows {
		d.Backups = append(d.Backups, backupRowFrom(b))
	}
	if len(d.Backups) == 0 && d.Error == "" {
		d.Msg = "Backup triggered."
	}
	c.Views.Fragment(g, "backups", d)
}
