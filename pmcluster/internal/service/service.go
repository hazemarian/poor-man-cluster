// Package service defines the ports (interfaces) of the pmcluster domain.
//
// The package holds interfaces only — no implementation. Each interface is
// implemented by an adapter in internal/service/impl (or by an existing
// package, e.g. internal/deploy). Consumers (CLI, daemon API, webhook,
// console) depend on these interfaces and never touch the implementation
// directly, so any port can be backed by the local core or by the daemon's
// REST API without changing the consumer.
package service
