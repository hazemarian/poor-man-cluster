package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/configs"

// ConfigsService is the configs port. The interface now lives in the configs
// domain package; this alias keeps older importers compiling during the
// domain-first restructure.
type ConfigsService = configs.Service
