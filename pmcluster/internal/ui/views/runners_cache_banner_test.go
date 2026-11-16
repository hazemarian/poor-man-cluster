package views

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunnerCacheWarningIsLoudOnThePage is the "make it loud" half of the cache work.
//
// The measurement is verified elsewhere; what this pins is that an operator SEES it: the
// banner names the volume and both numbers, and it is on the page rather than buried in a
// row's tooltip. Without a runner cache warning the repository that fills the disk is the
// first thing anyone hears about it.
func TestRunnerCacheWarningIsLoudOnThePage(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	base := map[string]any{
		"Configured": true, "Enabled": true, "Known": true,
		"Org": "acme", "Group": "pmcluster", "Memory": "2560m",
		"Rows": nil, "Nodes": nil, "Selected": 1,
	}

	// No warning: nothing about caches appears.
	clean := map[string]any{}
	for k, v := range base {
		clean[k] = v
	}
	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, "runners", clean); err != nil {
		t.Fatalf("render runners: %v", err)
	}
	if strings.Contains(buf.String(), "cache-over-title-marker") {
		t.Fatal("a warning banner rendered with no warnings")
	}

	// With a warning: the banner is present, it carries the tone that makes it visible,
	// and it repeats the text the operator has to act on.
	warned := map[string]any{}
	for k, v := range base {
		warned[k] = v
	}
	warned["CacheWarnings"] = []string{
		`go cache is 6.2GB (limit 5.4GB) in volume pmc-nxt-sw-6-w-org-cache-go — prune it or raise the limit, or this node will fill up`,
	}
	warned["CacheWarningText"] = warned["CacheWarnings"].([]string)[0]

	buf.Reset()
	if err := r.tmpl.ExecuteTemplate(&buf, "runners", warned); err != nil {
		t.Fatalf("render runners with a warning: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`banner warn`,                 // the amber banner, not an info line
		"6.2GB",                       // the measurement
		"pmc-nxt-sw-6-w-org-cache-go", // the volume to prune
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the cache banner is missing %q:\n%s", want, out)
		}
	}
}
