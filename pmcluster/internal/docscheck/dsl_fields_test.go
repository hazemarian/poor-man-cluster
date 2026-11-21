package docscheck

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// The manifest reference drifted exactly once in the way this test prevents: `health_timeout` shipped
// in pkg/dsl/types.go and appeared in no document at all — not in docs/dsl.md's field tables, not in
// the examples, nowhere. It was findable only by reading a CHANGELOG entry, which is not a reference.
//
// So: every JSON field of the public schema must be named somewhere in docs/dsl.md. "Named" is
// deliberately loose — a backticked occurrence anywhere in the file — because the file documents some
// fields in their own sections (`depends_on`, with a page of Swarm explanation) rather than in a table,
// and a stricter rule would push a real explanation out of the document to satisfy a checker.

// dslFields returns the JSON names of the struct's fields, recursively for the top level only:
// nested structs (expose.port, healthcheck.test, resources.limits) are documented as sub-keys of their
// parent and are covered by the parent's entry.
func dslFields(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// TestDSLFieldsAreDocumented fails when a manifest field exists in the schema and nowhere in the
// reference. The fix is a docs edit, never an allowlist entry: an exemption here is how the next
// undocumented field gets in.
func TestDSLFieldsAreDocumented(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "dsl.md"))
	if err != nil {
		t.Fatalf("read docs/dsl.md: %v", err)
	}
	text := string(doc)

	fields := map[string][]string{
		"app":     dslFields(t, dsl.App{}),
		"service": dslFields(t, dsl.Service{}),
	}

	var missing []string
	for _, level := range []string{"app", "service"} {
		for _, f := range fields[level] {
			// Backticked `field` anywhere in the reference — table row, prose or example.
			if !regexp.MustCompile("`" + regexp.QuoteMeta(f) + "`").MatchString(text) {
				missing = append(missing, level+"."+f)
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("undocumented manifest fields: %v\n"+
			"docs/dsl.md is the manifest reference; a field a customer can set but cannot read about "+
			"is a field they will get wrong. Add each to the field table (or to the section that "+
			"explains it) — the checker accepts a backticked mention anywhere in the file.", missing)
	}
}

// TestDSLFieldsHaveNoDuplicateTableRows catches the other drift: a field documented twice IN THE SAME
// TABLE with different rules. Scoped per table on purpose — `volumes`, `env` and `networks` are
// legitimately documented twice in the file, once at app level and once per service, and those are
// two different fields with different rules.
func TestDSLFieldsHaveNoDuplicateTableRows(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "dsl.md"))
	if err != nil {
		t.Fatalf("read docs/dsl.md: %v", err)
	}
	row := regexp.MustCompile("^\\|\\s*`([a-z_]+)`\\s*\\|")
	var dupes []string
	// One table = consecutive rows; a blank line or any non-row line ends it.
	for _, block := range strings.Split(string(doc), "\n\n") {
		seen := map[string]int{}
		for _, line := range strings.Split(block, "\n") {
			if m := row.FindStringSubmatch(line); m != nil {
				seen[m[1]]++
			}
		}
		for name, n := range seen {
			if n > 1 {
				dupes = append(dupes, name+" ("+strconv.Itoa(n)+" rows in one table)")
			}
		}
	}
	if len(dupes) > 0 {
		t.Fatalf("fields with more than one table row in docs/dsl.md: %v\n"+
			"Two rows for one field means two sets of rules, and the reader has no way to know which "+
			"one the code implements.", dupes)
	}
}
