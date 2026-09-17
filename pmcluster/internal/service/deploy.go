package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks"

// DeployService deploys, rolls back and removes application stacks. It is the
// write side of the stacks domain, aliased to stacks.Deployer while consumers
// migrate to the domain package.
type DeployService = stacks.Deployer
