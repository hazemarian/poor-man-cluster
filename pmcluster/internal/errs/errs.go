// Package errs owns the canonical domain error sentinels shared across
// package boundaries. It is a leaf package (stdlib-only) so the store, the
// configs/secrets domains, the remote adapters, and the consumers can all
// reference the SAME sentinel without creating an import cycle. The store
// and the domain packages alias these into their own namespaces
// (e.g. store.ErrConfigNotFound = errs.ErrConfigNotFound) so errors.Is works
// for either name.
package errs

import "errors"

var (
	// ErrConfigNotFound is returned when a config row does not exist.
	ErrConfigNotFound = errors.New("config not found")
	// ErrSecretNotFound is returned when a secret row does not exist.
	ErrSecretNotFound = errors.New("secret not found")
	// ErrStackNotFound is returned when a stack row does not exist.
	ErrStackNotFound = errors.New("stack not found")
	// ErrRevisionNotFound is returned when a stack revision does not exist.
	ErrRevisionNotFound = errors.New("revision not found")
	// ErrCredentialNotFound is returned when a managed credential does not exist.
	ErrCredentialNotFound = errors.New("credential not found")
	// ErrBackupNotFound is returned when a backup row does not exist.
	ErrBackupNotFound = errors.New("backup not found")
)
