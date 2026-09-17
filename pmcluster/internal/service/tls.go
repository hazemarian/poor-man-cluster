package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/certs"

// TLSService is the TLS certificate management port. It is the pre-domain
// alias for certs.Service (see internal/certs).
type TLSService = certs.Service
