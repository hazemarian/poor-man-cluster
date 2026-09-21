package controllers

import (
	"sort"

	"github.com/gin-gonic/gin"
)

// Usage answers "what breaks if I delete this": the daemon computes the
// reference graph from every stack's latest rendered compose (GET /api/usage),
// and the page lists each config and secret with the stacks that mount it.
//
// Two answers stay apart here: Known (the daemon answered — an entry with no
// stacks really is unused, which is actionable) and a failed read (the graph is
// unknown, and the page says so instead of drawing an empty list).
type Usage struct{ *Controller }

// usageRow is one config or secret and the stacks referencing it.
type usageRow struct {
	Name   string
	Stacks []string
	Count  int64
	Unused bool
}

// usageGroup is one table on the page. Configs and secrets have the same
// columns, the same empty state and the same unused marker, so they share the
// rendering template and differ only in the keys and the rows they carry.
type usageGroup struct {
	TitleKey string
	SubKey   string
	Rows     []usageRow
	Total    int64
	Unused   int64
}

type usageData struct {
	Configs usageGroup
	Secrets usageGroup

	Known  bool
	Unused int64

	ErrKey string
	ErrRaw string
}

// Page renders the usage graph (viewer role, no mutations).
func (c Usage) Page(g *gin.Context) {
	ctx := g.Request.Context()
	d := usageData{
		Configs: usageGroup{TitleKey: "usage.configs_title", SubKey: "usage.configs_sub"},
		Secrets: usageGroup{TitleKey: "usage.secrets_title", SubKey: "usage.secrets_sub"},
	}
	if _, _, configured := c.loadParams(ctx); !configured {
		d.ErrKey = "err.api_not_configured"
		c.Views.Fragment(g, "usage", d)
		return
	}
	u, err := c.API.GetUsage(ctx)
	if err != nil {
		d.ErrKey, d.ErrRaw = "err.usage", err.Error()
		c.Views.Fragment(g, "usage", d)
		return
	}
	d.Known = true
	d.Configs.Rows, d.Configs.Total, d.Configs.Unused = usageGroupRows(u.Configs)
	d.Secrets.Rows, d.Secrets.Total, d.Secrets.Unused = usageGroupRows(u.Secrets)
	d.Unused = d.Configs.Unused + d.Secrets.Unused
	c.Views.Fragment(g, "usage", d)
}

// usageGroupRows renders a name → stacks map as a name-sorted list: table order
// must not depend on Go's map iteration, or two loads of the same page would
// disagree.
func usageGroupRows(m map[string][]string) (rows []usageRow, total, unused int64) {
	rows = make([]usageRow, 0, len(m))
	for name, stacks := range m {
		sorted := append([]string(nil), stacks...)
		sort.Strings(sorted)
		rows = append(rows, usageRow{
			Name: name, Stacks: sorted, Count: int64(len(sorted)), Unused: len(sorted) == 0,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, int64(len(rows)), countUnused(rows)
}

// countUnused counts the entries no stack references.
func countUnused(rows []usageRow) int64 {
	var n int64
	for _, r := range rows {
		if r.Unused {
			n++
		}
	}
	return n
}
