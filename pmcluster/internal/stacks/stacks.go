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

import "context"

// Payload is the canonical deploy request. JSON shape is shared by the REST
// handler and the webhook receiver.
type Payload struct {
	AppName  string `json:"app_name,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
	Version  string `json:"version,omitempty"`
	Manifest string `json:"manifest"`
}

// Result is the outcome of a deploy or rollback.
type Result struct {
	StackName    string
	Revision     int64
	RenderedYAML []byte
}

// Stack is a deployed stack's metadata. RepoURL is empty when unset.
type Stack struct {
	Name            string
	CurrentRevision int64
	RepoURL         string
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
}

// Deployer is the write side: deploy, sync, roll back and remove stacks.
type Deployer interface {
	Deploy(ctx context.Context, p Payload) (*Result, error)
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
