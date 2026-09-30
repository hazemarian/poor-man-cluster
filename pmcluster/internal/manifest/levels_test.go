package manifest

import (
	"reflect"
	"strings"
	"testing"
)

func svc(name string, deps ...string) IRService {
	return IRService{Name: name, DependsOn: deps}
}

// TestServiceLevels_LinearChain: a depends on b, b depends on c → levels
// [c], [b], [a].
func TestServiceLevels_LinearChain(t *testing.T) {
	ir := &IR{Services: []IRService{
		svc("a", "b"),
		svc("b", "c"),
		svc("c"),
	}}
	levels, err := ServiceLevels(ir)
	if err != nil {
		t.Fatalf("ServiceLevels: %v", err)
	}
	want := [][]string{{"c"}, {"b"}, {"a"}}
	if !reflect.DeepEqual(levels, want) {
		t.Errorf("levels = %v, want %v", levels, want)
	}
}

// TestServiceLevels_FanOut: api + migration both depend on db → levels
// [db], [api migration] (order within a level follows IR order).
func TestServiceLevels_FanOut(t *testing.T) {
	ir := &IR{Services: []IRService{
		svc("db"),
		svc("migration", "db"),
		svc("api", "db"),
	}}
	levels, err := ServiceLevels(ir)
	if err != nil {
		t.Fatalf("ServiceLevels: %v", err)
	}
	want := [][]string{{"db"}, {"migration", "api"}}
	if !reflect.DeepEqual(levels, want) {
		t.Errorf("levels = %v, want %v", levels, want)
	}
}

// TestServiceLevels_MultiLevel: web depends on api depends on db →
// [db] [api] [web]; a no-dep service stays in level 0 with db.
func TestServiceLevels_MultiLevel(t *testing.T) {
	ir := &IR{Services: []IRService{
		svc("db"),
		svc("worker"), // no deps
		svc("api", "db"),
		svc("web", "api"),
	}}
	levels, err := ServiceLevels(ir)
	if err != nil {
		t.Fatalf("ServiceLevels: %v", err)
	}
	// Level 0 contains every zero-dependency service in IR order: db, worker.
	if len(levels) != 3 {
		t.Fatalf("levels = %v, want 3 levels", levels)
	}
	if !reflect.DeepEqual(levels[0], []string{"db", "worker"}) {
		t.Errorf("level 0 = %v, want [db worker]", levels[0])
	}
	if !reflect.DeepEqual(levels[1], []string{"api"}) || !reflect.DeepEqual(levels[2], []string{"web"}) {
		t.Errorf("levels = %v, want [[db worker] [api] [web]]", levels)
	}
}

// TestServiceLevels_CycleErrors: a depends on b, b depends on a → error.
func TestServiceLevels_CycleErrors(t *testing.T) {
	ir := &IR{Services: []IRService{
		svc("a", "b"),
		svc("b", "a"),
	}}
	_, err := ServiceLevels(ir)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("ServiceLevels(cycle) = %v, want a cycle error", err)
	}
}

// TestServiceLevels_UnknownDepErrors: migration depends on ghost → error.
func TestServiceLevels_UnknownDepErrors(t *testing.T) {
	ir := &IR{Services: []IRService{
		svc("db"),
		svc("migration", "ghost"),
	}}
	_, err := ServiceLevels(ir)
	if err == nil || !strings.Contains(err.Error(), "no such service") {
		t.Fatalf("ServiceLevels(unknown dep) = %v, want a no-such-service error", err)
	}
}

// TestServiceLevels_EmptyIR: no services → nil levels, no error.
func TestServiceLevels_EmptyIR(t *testing.T) {
	levels, err := ServiceLevels(&IR{})
	if err != nil {
		t.Fatalf("ServiceLevels(empty): %v", err)
	}
	if levels != nil {
		t.Errorf("levels = %v, want nil", levels)
	}
}

// TestSubset_FiltersAndRecomputes: a subset keeps only the named services and
// recomputes volumes + secrets from exactly those services.
func TestSubset_FiltersAndRecomputes(t *testing.T) {
	ir := &IR{
		Name:    "demo",
		Env:     "staging",
		Version: "v1",
		Services: []IRService{
			{Name: "db", Image: "postgres:14-alpine", Volumes: []string{"db_data:/var/lib/postgresql/data"}, Secrets: []string{"db_pass"}},
			{Name: "api", Image: "api:v1", Volumes: []string{"cache:/cache"}, Secrets: []string{"api_token"}},
		},
		Volumes: []string{"db_data", "cache"},
		Secrets: []string{"db_pass", "api_token"},
	}

	sub := ir.Subset([]string{"db"})
	if sub.Name != "demo" || sub.Env != "staging" || sub.Version != "v1" {
		t.Errorf("subset identity fields not copied: %+v", sub)
	}
	if len(sub.Services) != 1 || sub.Services[0].Name != "db" {
		t.Fatalf("subset services = %v, want only db", sub.Services)
	}
	if !reflect.DeepEqual(sub.Volumes, []string{"db_data"}) {
		t.Errorf("subset volumes = %v, want [db_data]", sub.Volumes)
	}
	if !reflect.DeepEqual(sub.Secrets, []string{"db_pass"}) {
		t.Errorf("subset secrets = %v, want [db_pass]", sub.Secrets)
	}
}
