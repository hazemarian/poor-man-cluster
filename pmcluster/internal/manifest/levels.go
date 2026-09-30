package manifest

import (
	"fmt"
	"strings"
)

// ServiceLevels topologically orders the IR's services into dependency
// levels from DependsOn. Every service in level N depends only on services
// in levels < N (a service with no dependencies is in level 0). The deploy
// pipeline deploys + waits for each level before the next — this is how
// startup ordering is enforced on Swarm, which parses and ignores compose
// depends_on entirely.
//
// Returns an error when a dependency name does not exist in the IR or when
// the dependency graph contains a cycle (both are unrecoverable at
// deploy time).
func ServiceLevels(ir *IR) ([][]string, error) {
	if len(ir.Services) == 0 {
		return nil, nil
	}
	index := make(map[string]int, len(ir.Services))
	for i := range ir.Services {
		index[ir.Services[i].Name] = i
	}

	// dependents[i] = services that depend on ir.Services[i].Name.
	dependents := make([][]string, len(ir.Services))
	// pending[i] = number of dependencies ir.Services[i] still waits on.
	pending := make([]int, len(ir.Services))
	for i := range ir.Services {
		for _, dep := range ir.Services[i].DependsOn {
			di, ok := index[dep]
			if !ok {
				return nil, fmt.Errorf("depends_on %q on service %q: no such service in the manifest", dep, ir.Services[i].Name)
			}
			pending[i]++
			dependents[di] = append(dependents[di], ir.Services[i].Name)
		}
	}

	var levels [][]string
	// Current level = services whose dependencies are all satisfied.
	cur := make([]string, 0, len(ir.Services))
	for i := range ir.Services {
		if pending[i] == 0 {
			cur = append(cur, ir.Services[i].Name)
		}
	}
	for len(cur) > 0 {
		levels = append(levels, cur)
		var next []string
		for _, name := range cur {
			for _, dependent := range dependents[index[name]] {
				pending[index[dependent]]--
				if pending[index[dependent]] == 0 {
					next = append(next, dependent)
				}
			}
		}
		cur = next
	}

	// Any service still pending after exhausting all levels is part of a
	// cycle (it never reached zero dependencies).
	for i := range ir.Services {
		if pending[i] > 0 {
			var cyc []string
			for j := range ir.Services {
				if pending[j] > 0 {
					cyc = append(cyc, ir.Services[j].Name)
				}
			}
			return nil, fmt.Errorf("depends_on cycle detected between services: %s", strings.Join(cyc, ", "))
		}
	}

	return levels, nil
}

// Subset returns a copy of the IR containing only the named services, with
// Volumes and Secrets recomputed from exactly those services. Writers can
// render any subset as a valid standalone compose (network + volume +
// secret declarations are derived per-service), which is what the ordered
// per-level deploy feeds to docker stack deploy.
func (ir *IR) Subset(names []string) *IR {
	out := &IR{
		Name:     ir.Name,
		Env:      ir.Env,
		Version:  ir.Version,
		Services: make([]IRService, 0, len(names)),
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	for _, svc := range ir.Services {
		if !want[svc.Name] {
			continue
		}
		out.Services = append(out.Services, svc)
		// Named volumes + secrets are recomputed from the subset's services.
		for _, v := range svc.Volumes {
			src := v
			if i := strings.IndexByte(src, ':'); i >= 0 {
				src = v[:i]
			}
			if src != "" && !strings.HasPrefix(src, "/") {
				out.Volumes = append(out.Volumes, src)
			}
		}
		out.Secrets = append(out.Secrets, svc.Secrets...)
	}
	return out
}
