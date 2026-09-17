package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/stacks"

// StacksService is the read side of deployed stacks, aliased to stacks.Reader
// while consumers migrate to the domain package.
type StacksService = stacks.Reader
