// Package noderuntime holds the loops a pmcluster node runs for its whole life.
//
// They are started by `pmcluster serve` and belong to the node, not to the command
// line: the runner assignment channel and its poll loop, the snapshot mirror a
// follower answers from, the volume-repair loop, the swarm leader watch and the
// adoption gate that answers 503 while the control plane is swapped, the docker
// watchdog, the node-stat sampler and the self-telemetry supervisor.
//
// Each takes a context, a configuration and its own collaborators — never a
// *cobra.Command — so a test drives the loop rather than a command. Nothing here is
// a composition root: internal/cli wires these into `serve`, and internal/nodeunits
// owns the systemd units that run the ones which must outlive a restart.
package noderuntime
