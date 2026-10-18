package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// InvestigationPolicyKey is the durable nonsecret model policy setting.
const InvestigationPolicyKey = "investigation_policy"

// ErrInvestigationBudget indicates disabled policy or exhausted admission capacity.
var ErrInvestigationBudget = errors.New("investigation budget unavailable")

// InvestigationReservation survives edge and daemon restarts.
type InvestigationReservation struct {
	ID       string `json:"id"`
	Bucket   string `json:"bucket"`
	Reserved int64  `json:"reserved"`
	Charged  int64  `json:"charged"`
	Status   string `json:"status"`
}

// InvestigationBudget is a UTC reservation-day token accounting snapshot.
type InvestigationBudget struct {
	Bucket  string `json:"bucket"`
	Held    int64  `json:"held"`
	Charged int64  `json:"charged"`
}

func reservationRow(row *sql.Row) (InvestigationReservation, error) {
	var r InvestigationReservation
	err := row.Scan(&r.ID, &r.Bucket, &r.Reserved, &r.Charged, &r.Status)
	return r, err
}

// ReserveInvestigation atomically admits the full policy allowance, never a client limit.
func (s *Store) ReserveInvestigation(ctx context.Context, id string) (InvestigationReservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InvestigationReservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := reservationRow(tx.QueryRowContext(ctx, `SELECT id,bucket,reserved,charged,status FROM investigation_reservations WHERE id=?`, id))
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM cluster_settings WHERE key=?`, InvestigationPolicyKey).Scan(&raw); err != nil {
		return r, ErrInvestigationBudget
	}
	var p struct {
		Enabled         bool  `json:"enabled"`
		Sharing         bool  `json:"sharing"`
		DailyTokens     int64 `json:"daily_tokens"`
		MaxInputTokens  int64 `json:"max_input_tokens"`
		MaxOutputTokens int64 `json:"max_output_tokens"`
	}
	if json.Unmarshal([]byte(raw), &p) != nil || !p.Enabled || !p.Sharing {
		return r, ErrInvestigationBudget
	}
	requested := p.MaxInputTokens + p.MaxOutputTokens
	if requested <= 0 || requested > 1_000_000 || p.DailyTokens < requested {
		return r, ErrInvestigationBudget
	}
	now := time.Now().UTC()
	bucket := now.Format("2006-01-02")
	var total int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status='settled' THEN charged ELSE reserved END),0) FROM investigation_reservations WHERE bucket=?`, bucket).Scan(&total); err != nil {
		return r, err
	}
	if total > p.DailyTokens-requested {
		return r, ErrInvestigationBudget
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO investigation_reservations(id,bucket,reserved,status,created_at,updated_at) VALUES(?,?,?,'reserved',?,?)`, id, bucket, requested, now.Unix(), now.Unix())
	if err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	return InvestigationReservation{ID: id, Bucket: bucket, Reserved: requested, Status: "reserved"}, nil
}

// DispatchInvestigation records possible provider spend before any network call.
func (s *Store) DispatchInvestigation(ctx context.Context, id string) (InvestigationReservation, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE investigation_reservations SET status='dispatched',updated_at=? WHERE id=? AND status='reserved'`, time.Now().Unix(), id)
	if err != nil {
		return InvestigationReservation{}, err
	}
	if affected, e := result.RowsAffected(); e != nil || affected != 1 {
		return InvestigationReservation{}, fmt.Errorf("reservation already dispatched or settled")
	}
	r, err := reservationRow(s.db.QueryRowContext(ctx, `SELECT id,bucket,reserved,charged,status FROM investigation_reservations WHERE id=?`, id))
	if err == nil && r.Status != "dispatched" {
		return r, fmt.Errorf("reservation cannot be dispatched")
	}
	return r, err
}

// SettleInvestigation releases only undispatched work; unknown spend charges the allowance.
func (s *Store) SettleInvestigation(ctx context.Context, id string, usage *int64) (InvestigationReservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InvestigationReservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := reservationRow(tx.QueryRowContext(ctx, `SELECT id,bucket,reserved,charged,status FROM investigation_reservations WHERE id=?`, id))
	if err != nil {
		return r, err
	}
	charge := int64(0)
	if r.Status == "settled" {
		return r, nil
	}
	if r.Status == "dispatched" {
		charge = r.Reserved
		if usage != nil {
			if *usage < 0 || *usage > 1_000_000_000 {
				return r, fmt.Errorf("invalid usage")
			}
			charge = *usage
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE investigation_reservations SET status='settled',charged=?,updated_at=? WHERE id=?`, charge, time.Now().Unix(), id)
	if err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	r.Status = "settled"
	r.Charged = charge
	return r, nil
}

// InvestigationBudget reports held reservations separately from confirmed charges.
func (s *Store) InvestigationBudget(ctx context.Context) (InvestigationBudget, error) {
	b := InvestigationBudget{Bucket: time.Now().UTC().Format("2006-01-02")}
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status!='settled' THEN reserved ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='settled' THEN charged ELSE 0 END),0) FROM investigation_reservations WHERE bucket=?`, b.Bucket).Scan(&b.Held, &b.Charged)
	return b, err
}
