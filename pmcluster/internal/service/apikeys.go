package service

import (
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/apikeys"
)

// APIKeysService is retained as an alias for the apikeys domain port while
// consumers migrate to apikeys.Service directly.
type APIKeysService = apikeys.Service

var (
	// ErrEdgeUserProtected and ErrSelfDelete are aliases for the apikeys
	// domain sentinels (same error values, so errors.Is is unaffected).
	ErrEdgeUserProtected = apikeys.ErrEdgeUserProtected
	ErrSelfDelete        = apikeys.ErrSelfDelete
)