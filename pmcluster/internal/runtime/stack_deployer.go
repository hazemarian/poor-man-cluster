package runtime

import "context"

// StackDeployer applies a compose file to the swarm under a given stack
// name. It is the deploy-port contract the app-stack package depends on; it
// lives here (stdlib-only leaf) so internal/stacks never has to reach into
// the platform-lifecycle package (internal/cluster) for its dependency.
// internal/cluster aliases this type (type StackDeployer = runtime.StackDeployer)
// so the production CLI deployer and its existing references keep working
// unchanged.
type StackDeployer interface {
	// DeployStack applies the compose file and then reconciles the stack:
	// drift-prune services dropped from the compose and force-update every
	// remaining service. Used for full-stack deploys (cluster up/update and
	// single-level app stacks).
	DeployStack(ctx context.Context, name string, composeYAML []byte) error
	// DeployStackNoPrune applies the compose file WITHOUT pruning or
	// force-updating. Used by the ordered per-level app-stack deploy: each
	// depends_on level is deployed separately, so pruning mid-way would
	// remove not-yet-deployed sibling services.
	DeployStackNoPrune(ctx context.Context, name string, composeYAML []byte) error
	// PruneStack removes live stack services that are not declared in the
	// compose file. Run once at the end of an ordered per-level deploy with
	// the full stack compose.
	PruneStack(ctx context.Context, name string, composeYAML []byte) error
	RemoveStack(ctx context.Context, name string) error

	ForceUpdateService(ctx context.Context, fullName string) error

	PruneStaleContainers(ctx context.Context, stackName string, olderThan string) error
}
