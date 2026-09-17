// Package service is a compatibility shim for the domain packages.
// New code should import the domain packages directly (apikeys, webhooks,
// secrets, configs, backups, certs, stacks, cluster).
package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/backups"

// ErrBackupTriggerNotConfigured is returned by BackupsService.Trigger when no
// backup trigger is wired. The daemon always wires one; consumers that only
// list backups can run without one.
var ErrBackupTriggerNotConfigured = backups.ErrTriggerNotConfigured

// BackupsService triggers and lists backups of stack volumes.
type BackupsService = backups.Service
