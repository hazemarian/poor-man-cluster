package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file guards the CLI/noderuntime/nodeunits split. It is not a style test: it
// exists because the split was made twice (the host units first, then the loops
// serve starts) and each time the failure mode was the same — a helper left behind
// in the wrong package, or a copy of one written beside the caller instead of
// imported, after which the two drift and only one of them gets fixed.
//
// Two assertions, both chosen over a nicer one that does not survive contact with
// the code: "every file in internal/cli defines a command" would need eleven
// exemptions today (store_open.go, daemon.go, converge.go, telemetry.go, …), and an
// exemption list nobody re-reads is how a boundary re-erodes. What is checked here
// is what can be stated without a list of excuses.

// packages that must not define the same top-level name twice.
var splitPackages = []string{".", "../noderuntime", "../nodeunits"}

// topLevelNames maps every top-level func, type, var and const declared in the
// package's non-test files to the file that declares it.
func topLevelNames(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := map[string]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		record := func(id *ast.Ident) {
			if id == nil || id.Name == "_" {
				return
			}
			names[id.Name] = name
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				// Methods live in the method set of their receiver, so two types may
				// share a method name; only package-level functions are one-per-package.
				if d.Recv == nil {
					record(d.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						record(s.Name)
					case *ast.ValueSpec:
						for _, id := range s.Names {
							record(id)
						}
					}
				}
			}
		}
	}
	return names
}

// TestAPackageSymbolHasOneOwner fails when internal/cli, internal/noderuntime and
// internal/nodeunits each declare the same name.
//
// The failure this prevents: `localRetentionPolicy` reimplemented in the CLI because
// importing the package that owns it felt like overhead. Both versions compile, one
// of them is wrong, and nothing says which.
func TestAPackageSymbolHasOneOwner(t *testing.T) {
	owners := map[string][]string{}
	for _, dir := range splitPackages {
		for name, file := range topLevelNames(t, dir) {
			owners[name] = append(owners[name], filepath.Join(filepath.Clean(dir), file))
		}
	}
	var clashes []string
	for name, where := range owners {
		if len(where) > 1 {
			sort.Strings(where)
			clashes = append(clashes, name+": "+strings.Join(where, ", "))
		}
	}
	if len(clashes) > 0 {
		sort.Strings(clashes)
		t.Fatalf("these names are defined in more than one of internal/cli, internal/noderuntime and internal/nodeunits — one owner each, or the copies drift and only one gets fixed:\n  %s",
			strings.Join(clashes, "\n  "))
	}
}

// TestTheNodeRuntimeIsNotBackInTheCLI pins the specific move: the loops `serve`
// starts live in internal/noderuntime and nowhere else.
//
// TestAPackageSymbolHasOneOwner catches a COPY; this catches the other direction — a
// loop quietly re-added to the CLI, or the whole file set moved back, which would
// leave the two guards above green because nothing is duplicated.
func TestTheNodeRuntimeIsNotBackInTheCLI(t *testing.T) {
	cliNames := topLevelNames(t, ".")
	runtimeNames := topLevelNames(t, "../noderuntime")

	// The entry points serve.go actually calls, plus the two probes that decide what
	// this node is. Each one moved out of the CLI; a regression puts it back.
	mustLiveInRuntime := []string{
		"StartPerNodeLoops",
		"StartSnapshotMirror",
		"StartAssignmentListener",
		"StartVolumeRepairLoop",
		"StartWorkerAPI",
		"StartNodeStatSampler",
		"WatchSwarmLeadership",
		"WaitForSwarmLeadership",
		"WatchDockerOnce",
		"PublishBackupPlan",
		"AdoptOnLeadershipGain",
		"ConfirmMirrorAtStartup",
		"IsSwarmLeader",
		"IsSwarmWorker",
		"IsStandby",
		"LoadWatchdogState",
		"SaveWatchdogState",
	}

	for _, name := range mustLiveInRuntime {
		if _, ok := runtimeNames[name]; !ok {
			t.Errorf("%s is not defined in internal/noderuntime — it is one of the loops serve starts, and the package exists so those are testable without a cobra command", name)
		}
		if file, ok := cliNames[name]; ok {
			t.Errorf("%s is defined in internal/cli/%s — the node runtime belongs in internal/noderuntime, and a second copy in the CLI is how the two drift", name, file)
		}
	}
}
