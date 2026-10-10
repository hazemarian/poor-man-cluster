package secrets

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// HealStatus classifies one secret row in a heal pass.
type HealStatus string

const (
	// HealOK: the ciphertext decrypts with the current key, the stored hash
	// matches the plaintext, and (when a mirror is configured) the Docker
	// swarm object already exists.
	HealOK HealStatus = "ok"
	// HealRepaired: the row was healed — a stale hash was corrected and/or
	// the content-addressed swarm secret was created.
	HealRepaired HealStatus = "repaired"
	// HealDegraded: the row decrypts, but its swarm mirror could not be
	// created. A deploy referencing it (or a failover restore) will fail
	// until that is fixed.
	HealDegraded HealStatus = "degraded"
	// HealUnrecoverable: the ciphertext does not decrypt with the current
	// key — it was sealed by an older .encryption_key. AES-GCM cannot be
	// reversed without the key that sealed it, so the value must be
	// re-supplied by the operator.
	HealUnrecoverable HealStatus = "unrecoverable"
)

// HealEntry is the per-secret outcome of a heal pass.
type HealEntry struct {
	Name  string
	Scope string
	Stack string
	// Hash is the sha256 hex of the plaintext when it could be decrypted
	// (the repaired value when the stored hash was stale); empty otherwise.
	Hash string
	// Status is the entry's classification.
	Status HealStatus
	// Actions lists, in order, what the pass did or found.
	Actions []string
}

// HealReport is the outcome of a heal pass over every stored secret.
type HealReport struct {
	Entries       []HealEntry
	OK            int
	Repaired      int
	Degraded      int
	Unrecoverable int
}

// HealOptions controls a heal pass.
type HealOptions struct {
	// DryRun reports what would change without writing to the store or
	// creating any Docker object.
	DryRun bool
	// Mirror materializes a decrypted value as a Docker swarm secret
	// (content-addressed — see store.SwarmSecretName) and reports whether it
	// created a new object. nil skips mirroring entirely (no daemon
	// reachable). A mirror error is recorded per entry; it never aborts the
	// pass.
	Mirror func(ctx context.Context, name, value, hash string) (created bool, err error)
}

// Heal walks every stored secret and makes it deployable again:
//
//  1. decrypt the row with the current key — a row that does not decrypt was
//     written under an older .encryption_key and must be re-supplied;
//  2. repair a stale hash (the stored hash is what translation uses to derive
//     the Docker swarm secret name, so a mismatch references an object that
//     does not exist);
//  3. materialize the content-addressed swarm secret so `docker stack deploy`
//     can mount it.
//
// A heal pass never changes a secret's VALUE.
func (s *Local) Heal(ctx context.Context, opts HealOptions) (*HealReport, error) {
	rows, err := s.Store.ListSecrets(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	rep := &HealReport{Entries: make([]HealEntry, 0, len(rows))}
	for _, meta := range rows {
		row, err := s.Store.Secret(ctx, meta.Name)
		if err != nil {
			return nil, fmt.Errorf("read secret %s: %w", meta.Name, err)
		}
		e := HealEntry{Name: row.Name, Scope: row.Scope, Stack: row.Stack}

		plain, derr := s.Cipher.Decrypt(row.Payload)
		if derr != nil {
			e.Status = HealUnrecoverable
			e.Actions = append(e.Actions,
				"ciphertext does not decrypt with the current encryption key (sealed by an older key)",
				fmt.Sprintf("re-supply the value: printf '%%s' '<value>' | pmcluster secret edit %s", row.Name))
			rep.Unrecoverable++
			rep.Entries = append(rep.Entries, e)
			continue
		}

		value := string(plain)
		hash := store.SecretHash(value)
		e.Hash = hash
		changed := false

		if row.Hash != hash {
			if opts.DryRun {
				e.Actions = append(e.Actions,
					fmt.Sprintf("stale hash would be repaired (%s → %s)", hashPrefix(row.Hash), hashPrefix(hash)))
			} else {
				if err := s.Store.UpdateSecret(ctx, row.Name, row.Payload, hash); err != nil {
					return nil, fmt.Errorf("repair hash for %s: %w", row.Name, err)
				}
				e.Actions = append(e.Actions,
					fmt.Sprintf("stale hash repaired (%s → %s)", hashPrefix(row.Hash), hashPrefix(hash)))
			}
			changed = true
		}

		mirrorFailed := false
		if opts.Mirror != nil {
			target := store.SwarmSecretName(row.Name, hash)
			if opts.DryRun {
				e.Actions = append(e.Actions, "swarm mirror would be ensured ("+target+")")
			} else {
				created, merr := opts.Mirror(ctx, row.Name, value, hash)
				switch {
				case merr != nil:
					mirrorFailed = true
					e.Actions = append(e.Actions,
						fmt.Sprintf("swarm mirror %s could not be created: %v", target, merr))
				case created:
					changed = true
					e.Actions = append(e.Actions, "swarm mirror created ("+target+")")
				default:
					e.Actions = append(e.Actions, "swarm mirror already present")
				}
			}
		}

		switch {
		case mirrorFailed:
			e.Status = HealDegraded
			rep.Degraded++
		case changed:
			e.Status = HealRepaired
			rep.Repaired++
		default:
			e.Status = HealOK
			rep.OK++
		}
		rep.Entries = append(rep.Entries, e)
	}
	return rep, nil
}

// hashPrefix shortens a sha256 hex for display (matching the swarm
// content-addressed suffix length).
func hashPrefix(h string) string {
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}
