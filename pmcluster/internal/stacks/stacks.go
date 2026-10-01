// Package stacks is the bounded context for deployed application stacks. It
// owns the deploy engine (the pipeline that turns a DSL manifest into a
// running Docker Swarm stack) together with the read side (stack metadata and
// revision history). Models, ports and adapters all live here.
//
// The deploy pipeline:
//
//	Payload → Parse → (override version) → Interpolate → Validate
//	        → Translate → RecordDeploy → DeployStack
package stacks

import (
	"context"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Payload is the canonical deploy request. JSON shape is shared by the REST
// handler and the webhook receiver.
type Payload struct {
	AppName string `json:"app_name,omitempty"`
	RepoURL string `json:"repo_url,omitempty"`
	// File is the manifest path inside the source repo (e.g. deploy/test-lms.yaml).
	// Recorded for provenance so a revision can be traced back to its source.
	File     string `json:"file,omitempty"`
	Version  string `json:"version,omitempty"`
	Manifest string `json:"manifest"`
}

// Result is the outcome of a deploy or rollback.
type Result struct {
	StackName    string
	Revision     int64
	RenderedYAML []byte
	// Changed is false when a sync re-translated the latest source manifest
	// and the rendered compose matched the stored rendered_hash — nothing
	// was deployed and no new revision was recorded.
	Changed bool
}

// Stack is a deployed stack's metadata. RepoURL is empty when unset; SourceFile
// is the manifest path inside that repo (deploy/test-lms.yaml). StackErrors is
// the deploy/apply outcome history, newest first (each entry carries the
// revision it applied to; an empty Error means that apply succeeded) —
// fire-and-forget deploys apply in the background, so failures surface here
// for the console instead of the response.
type Stack struct {
	Name            string
	CurrentRevision int64
	RepoURL         string
	SourceFile      string
	StackErrors     []store.StackErrorEntry
	CreatedAt       int64
	UpdatedAt       int64
}

// Revision is one stored deployment of a stack. PayloadJSON carries the
// serialized deploy payload (or a rollback marker) and is empty when unset.
type Revision struct {
	StackName    string
	Revision     int64
	SourceYAML   string
	RenderedYAML string
	PayloadJSON  string
	CreatedAt    int64
	// Error is this deployment execution's outcome (joined from the stack's
	// error history by revision): "" = applied cleanly, else the failure.
	Error string
}

// Deployer is the write side: deploy, sync, roll back and remove stacks.
type Deployer interface {
	Deploy(ctx context.Context, p Payload) (*Result, error)
	// DeployAsync validates + records the revision synchronously, then applies
	// the swarm deploy in the background. Used by the webhook receiver and the
	// API deploy handler so callers can return early (202) while a slow stack
	// (e.g. a cold postgres that takes minutes to become healthy) converges.
	DeployAsync(ctx context.Context, p Payload) (*Result, error)
	// Sync re-runs the deploy pipeline for an existing stack from its latest
	// stored source manifest, re-resolving config()/secrets() references
	// against the DB. Used by the console "sync" button (k8s-style reconcile).
	Sync(ctx context.Context, stackName string) (*Result, error)
	Rollback(ctx context.Context, stackName string, sourceRevision int64) (*Result, error)
	Undeploy(ctx context.Context, stackName string) error
}

// Reader is the read side: stack metadata and revision history.
type Reader interface {
	Get(ctx context.Context, name string) (*Stack, error)
	List(ctx context.Context) ([]Stack, error)
	Revisions(ctx context.Context, name string, limit int) ([]Revision, error)
}
