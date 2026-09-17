package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/secrets"

// SecretsService is the business entry point for the encrypted secrets store.
// It is the domain port defined by internal/secrets.
type SecretsService = secrets.Service
