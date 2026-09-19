package controllers

import (
	"strconv"

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

type backupBrowseData struct {
	Run   *pmapi.Backup
	Files []pmapi.BackupFile
	Error string
	Msg   string
}

// Browse renders the file listing of one backup run.
func (c Backups) Browse(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := backupBrowseData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "backupbrowse", d)
		return
	}
	id, err := strconv.ParseInt(g.Param("id"), 10, 64)
	if err != nil {
		d.Error = "invalid backup id"
		c.Views.Fragment(g, "backupbrowse", d)
		return
	}
	run, files, err := c.API.BrowseBackup(ctx, id)
	if err != nil {
		d.Error = err.Error()
		c.Views.Fragment(g, "backupbrowse", d)
		return
	}
	d.Run, d.Files = run, files
	c.Views.Fragment(g, "backupbrowse", d)
}

// Restore extracts a backup run's archives back into the volume root and
// re-renders the browse view.
func (c Backups) Restore(g *gin.Context) {
	ctx := g.Request.Context()
	_, _, configured := c.loadParams(ctx)
	d := backupBrowseData{}
	if !configured {
		d.Error = "pmcluster API not configured. Open Settings first."
		c.Views.Fragment(g, "backupbrowse", d)
		return
	}
	id, err := strconv.ParseInt(g.Param("id"), 10, 64)
	if err != nil {
		d.Error = "invalid backup id"
		c.Views.Fragment(g, "backupbrowse", d)
		return
	}
	n, err := c.API.RestoreBackup(ctx, id, "/var/stack/data")
	if err != nil {
		d.Error = err.Error()
	} else {
		d.Msg = "Restored " + strconv.Itoa(n) + " file(s) under /var/stack/data."
	}
	run, files, err := c.API.BrowseBackup(ctx, id)
	if err != nil && d.Error == "" {
		d.Error = err.Error()
	}
	d.Run, d.Files = run, files
	c.Views.Fragment(g, "backupbrowse", d)
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
