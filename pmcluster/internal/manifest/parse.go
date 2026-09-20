// Package manifest is the DSL pipeline:
//  1. Parse       — YAML → *dsl.App, strict (unknown keys = error)
//  2. Interpolate — ${app} / ${env} / ${version} / ${registry} / ${env:VAR}
//  3. Validate    — semantic checks (image required, expose.port valid, …)
//  4. Translate   — *dsl.App → compose YAML
package manifest

import (
	"bytes"
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// Parse is strict — unknown keys are errors so typos like `repalicas: 2`
// fail loud. Caller must follow up with Interpolate then Validate.
func Parse(data []byte) (*dsl.App, error) {
	jsonData, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	var app dsl.App
	dec := jsonDecoder(bytes.NewReader(jsonData))
	if err := dec.Decode(&app); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &app, nil
}
