package manifest

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// InterpolateValidate runs the shared Interpolate → Validate steps against an
// already-parsed *dsl.App. It is the single place these two steps are ordered,
// so a step added between them (or reordered) reaches every caller. Errors are
// wrapped with the failing stage ("interpolate"/"validate").
func InterpolateValidate(app *dsl.App) error {
	if err := Interpolate(app); err != nil {
		return fmt.Errorf("interpolate: %w", err)
	}
	if err := Validate(app); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

// ParseBuild is the single shared pipeline for turning a raw DSL source
// manifest into a built IR: Parse → (optional name override) → Interpolate →
// Validate → BuildIR. Every deploy/sync/rollback/render path that starts from
// raw source goes through here, so a pipeline step added once cannot be missed
// at another site. Errors are wrapped with the failing stage ("parse" /
// "interpolate" / "validate" / "build"); callers add their own context.
//
// nameOverride, when non-empty, replaces the app name after parse (mirroring
// the AppName override deploy/sync/rollback apply) so ${app} interpolation and
// validation see the overridden name.
func ParseBuild(ctx context.Context, source []byte, nameOverride string, res EnvResolver) (*dsl.App, *IR, error) {
	app, err := Parse(source)
	if err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	if nameOverride != "" {
		app.Name = nameOverride
	}
	if err := InterpolateValidate(app); err != nil {
		return nil, nil, err
	}
	ir, err := BuildIR(ctx, app, res)
	if err != nil {
		return nil, nil, fmt.Errorf("build: %w", err)
	}
	return app, ir, nil
}

// RenderStamped renders an IR twice — once label-free to derive the content
// hash, then again stamped with runtime.RenderedHashLabel = hash(plain) — and
// returns the LABELED bytes. mk builds a writer for a given extra-label set;
// hash computes the rendered-hash label value from the label-free bytes
// (stackdrift.ContentHash for app stacks, store.ConfigHash for platform stacks
// — byte-identical algorithms). This is the single implementation of the
// two-pass stamped render shared by app and platform stacks.
func RenderStamped(ctx context.Context, ir *IR, mk func(extra map[string]string) *ComposeWriter, hash func([]byte) string) ([]byte, error) {
	plain, err := mk(nil).Write(ctx, ir)
	if err != nil {
		return nil, err
	}
	return mk(map[string]string{runtime.RenderedHashLabel: hash(plain)}).Write(ctx, ir)
}
