package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pmcdocs "github.com/hazemarian/poor-man-cluster/pmcluster/docs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability/parseable"
)

// The instructions are the one thing a client reads before any tool call, so they
// must name the contract and the traps that are expensive to discover.
func TestInstructionsStateTheContract(t *testing.T) {
	catalog := append(mustCatalog(t), curatedTools()...)
	got := instructions(Options{}, catalog)

	wants := []string{
		"manual_action_required",               // how a refusal arrives
		"do_not_retry",                         // what to do with it
		"pmc_update_config",                    // the one permitted write
		"202",                                  // deploy is async
		"IDLE fraction",                        // the CPU inversion
		"1 - value",                            //
		"pmcluster://schema/api",               // resources exist
		"pmcluster://policy",                   //
		"Parseable",                            // where the rest lives
		observability.MetricNodeCPUUtilization, // metric name, from the package
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("instructions never mention %q", w)
		}
	}
	if strings.Contains(got, "DANGEROUS: never served through MCP") {
		t.Error("instructions and tool descriptions are different registers; the tier marker belongs in descriptions")
	}
}

// The log schema must describe the projection the adapter actually sends.
func TestLogSchemaMatchesTheAdapterProjection(t *testing.T) {
	client, err := parseable.New(parseable.Config{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("parseable.New: %v", err)
	}
	end := time.Now().UTC()
	q, err := client.LogsSQL(observability.Query{
		Window: observability.Window{Start: end.Add(-15 * time.Minute), End: end},
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("LogsSQL: %v", err)
	}
	schema := logsSchema()
	for _, col := range logColumns {
		if !strings.Contains(q, col) {
			t.Errorf("adapter SQL no longer selects %q, but the published schema says it does:\n%s", col, q)
		}
		if !strings.Contains(schema, col) {
			t.Errorf("published log schema omits %q", col)
		}
	}
	// Every selected column must be documented, so a new projection column cannot
	// appear without the schema being updated.
	selectPart := q[strings.Index(q, "SELECT")+len("SELECT") : strings.Index(q, " FROM")]
	for _, field := range strings.Split(selectPart, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		name := strings.Trim(field[strings.LastIndex(field, " ")+1:], "`")
		if !strings.Contains(schema, name) {
			t.Errorf("adapter selects %q, which the published schema does not mention", name)
		}
	}
}

func TestResourcesArePublishedWithBodies(t *testing.T) {
	catalog := append(mustCatalog(t), curatedTools()...)
	docs := resources(Options{}, catalog, pmcdocs.OpenAPI)
	uris := map[string]string{}
	for _, d := range docs {
		if d.res.URI == "" || d.res.Name == "" || d.res.Description == "" {
			t.Errorf("resource %q is incomplete: %+v", d.res.URI, d.res)
		}
		if strings.TrimSpace(d.body) == "" {
			t.Errorf("resource %q has an empty body", d.res.URI)
		}
		uris[d.res.URI] = d.body
	}
	for _, want := range []string{"pmcluster://schema/api", "pmcluster://schema/logs", "pmcluster://schema/metrics", "pmcluster://policy"} {
		if _, ok := uris[want]; !ok {
			t.Errorf("resource %s is not published", want)
		}
	}
	if !strings.Contains(uris["pmcluster://schema/metrics"], observability.MetricContainerMemoryPercent) {
		t.Error("metrics resource omits a container metric name")
	}
	if !strings.Contains(uris["pmcluster://policy"], "pmc_update_config") {
		t.Error("policy resource does not name the permitted write")
	}
	if len(uris["pmcluster://schema/api"]) < 1000 {
		t.Error("api resource does not carry the spec")
	}
}

// Resources must be reachable over the wire, not just constructible.
func TestResourcesAreServedToAClient(t *testing.T) {
	cs := connect(t, &stubRouter{}, Options{Spec: pmcdocs.OpenAPI}, "Bearer t")
	var seen []string
	for r, err := range cs.Resources(context.Background(), nil) {
		if err != nil {
			t.Fatalf("resources: %v", err)
		}
		seen = append(seen, r.URI)
	}
	if len(seen) < 4 {
		t.Fatalf("client sees %d resources: %v", len(seen), seen)
	}
	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "pmcluster://schema/metrics"})
	if err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if len(res.Contents) == 0 || !strings.Contains(res.Contents[0].Text, observability.MetricNodeLoad1m) {
		t.Fatalf("metrics resource body is wrong: %+v", res.Contents)
	}
}

func mustCatalog(t *testing.T) []Tool {
	t.Helper()
	c, err := BuildCatalog(pmcdocs.OpenAPI)
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	return c
}

// Instructions must arrive on the wire at initialize, not merely exist as a
// function — a client that never receives them behaves exactly as if they were
// absent.
func TestInstructionsArriveAtInitialize(t *testing.T) {
	cs := connect(t, &stubRouter{}, Options{Spec: pmcdocs.OpenAPI}, "Bearer t")
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("no initialize result")
	}
	if !strings.Contains(init.Instructions, "manual_action_required") {
		t.Errorf("instructions did not reach the client: %q", init.Instructions)
	}
	if !strings.Contains(init.Instructions, "pmc_update_config") {
		t.Errorf("instructions do not name the permitted write: %q", init.Instructions)
	}
}
