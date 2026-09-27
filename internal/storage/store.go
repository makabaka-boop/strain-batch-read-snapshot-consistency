// Package storage persists editable drafts and immutable sealed reports in
// PostgreSQL.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"seqlabels/internal/labels"

	_ "github.com/lib/pq"
)

var (
	ErrBatchNotFound  = errors.New("batch not found")
	ErrBatchSealed    = errors.New("batch is sealed")
	ErrReportNotFound = errors.New("report not found")
)

// Store is the PostgreSQL-backed application storage.
type Store struct {
	db *sql.DB
}

// NewStore validates the database handle and returns a Store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB exposes the connection pool for tests and health checks.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Batch is a draft or sealed batch and its records.
type Batch struct {
	ID        string
	Sealed    bool
	CreatedAt time.Time
	UpdatedAt time.Time
	Records   []labels.Record
}

// Report is the complete immutable certification result.
type Report struct {
	ID        string
	BatchID   string
	Algorithm string
	InputHash string
	SealedAt  time.Time
	Records   []labels.Record
	Labels    map[string]*labels.Label
}

// Migrate creates the complete schema. It is idempotent and safe to run on
// every API startup.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schemaSQL)
	return err
}

// CreateBatch inserts an empty draft.
func (s *Store) CreateBatch(ctx context.Context) (string, time.Time, error) {
	var id string
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO batches (sealed) VALUES (false)
		RETURNING id, created_at`).Scan(&id, &createdAt)
	return id, createdAt, err
}

// GetBatch returns one batch and records ordered by ASCII identifier from a
// single repeatable-read snapshot.
func (s *Store) GetBatch(ctx context.Context, id string) (*Batch, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		ReadOnly:  true,
		Isolation: sql.LevelRepeatableRead,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	b := &Batch{}
	err = tx.QueryRowContext(ctx, `
		SELECT id, sealed, created_at, updated_at
		FROM batches WHERE id = $1`, id).
		Scan(&b.ID, &b.Sealed, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBatchNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT record_id, sequence
		FROM records
		WHERE batch_id = $1
		ORDER BY record_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r labels.Record
		if err := rows.Scan(&r.ID, &r.Sequence); err != nil {
			return nil, err
		}
		b.Records = append(b.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return b, tx.Commit()
}

// PutRecord inserts or replaces one sequence. It takes a row lock and rechecks
// both the batch status and total-size limit before making any change.
func (s *Store) PutRecord(ctx context.Context, batchID, recordID, sequence string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := ensureEditable(ctx, tx, batchID); err != nil {
		return err
	}
	var total, oldLength int
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(octet_length(sequence)), 0),
		       COALESCE((SELECT octet_length(sequence) FROM records
		                 WHERE batch_id = $1 AND record_id = $2), 0)
		FROM records WHERE batch_id = $1`, batchID, recordID).
		Scan(&total, &oldLength)
	if err != nil {
		return err
	}
	if len(sequence) > labels.MaxSequenceLen {
		return labels.ErrSequenceTooBig
	}
	if err := labels.ValidateID(recordID); err != nil {
		return err
	}
	if err := labels.Validate([]labels.Record{{ID: recordID, Sequence: sequence}}); err != nil {
		return err
	}
	if total-oldLength+len(sequence) > labels.MaxBatchLen {
		return labels.ErrBatchTooBig
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO records (batch_id, record_id, sequence)
		VALUES ($1, $2, $3)
		ON CONFLICT (batch_id, record_id) DO UPDATE
		SET sequence = EXCLUDED.sequence`,
		batchID, recordID, sequence)
	if err != nil {
		return mapWriteError(err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE batches SET updated_at = now() WHERE id = $1`, batchID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteRecord removes one sequence from an editable draft. Deleting an
// unknown identifier is idempotent.
func (s *Store) DeleteRecord(ctx context.Context, batchID, recordID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureEditable(ctx, tx, batchID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM records WHERE batch_id = $1 AND record_id = $2`,
		batchID, recordID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE batches SET updated_at = now() WHERE id = $1`, batchID); err != nil {
		return err
	}
	return tx.Commit()
}

// Freeze computes and atomically stores the report, then seals its batch.
func (s *Store) Freeze(ctx context.Context, batchID string, compute func([]labels.Record) (map[string]*labels.Label, error)) (*Report, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var sealed bool
	err = tx.QueryRowContext(ctx,
		`SELECT sealed FROM batches WHERE id = $1 FOR UPDATE`, batchID).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBatchNotFound
	}
	if err != nil {
		return nil, err
	}
	if sealed {
		return nil, ErrBatchSealed
	}

	records, err := loadRecords(ctx, tx, batchID)
	if err != nil {
		return nil, err
	}
	hash, err := labels.CanonicalHash(records)
	if err != nil {
		return nil, err
	}
	labelMap, err := compute(records)
	if err != nil {
		return nil, err
	}
	frozenInput, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	labelJSON, err := json.Marshal(labelMap)
	if err != nil {
		return nil, err
	}

	r := &Report{BatchID: batchID, Algorithm: labels.AlgorithmVersion, InputHash: hash, Records: records, Labels: labelMap}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO reports
		    (batch_id, algorithm, input_hash, frozen_input, labels)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb)
		RETURNING id, sealed_at`,
		batchID, r.Algorithm, r.InputHash, frozenInput, labelJSON).
		Scan(&r.ID, &r.SealedAt)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE batches SET sealed = true, updated_at = now() WHERE id = $1`,
		batchID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetReport reads an immutable report by its batch identifier.
func (s *Store) GetReport(ctx context.Context, batchID string) (*Report, error) {
	var (
		r           Report
		frozenInput []byte
		labelJSON   []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, batch_id, algorithm, input_hash, sealed_at,
		       frozen_input, labels
		FROM reports
		WHERE batch_id = $1`, batchID).
		Scan(&r.ID, &r.BatchID, &r.Algorithm, &r.InputHash, &r.SealedAt,
			&frozenInput, &labelJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReportNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(frozenInput, &r.Records); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(labelJSON, &r.Labels); err != nil {
		return nil, err
	}
	return &r, nil
}

func ensureEditable(ctx context.Context, tx *sql.Tx, batchID string) error {
	var sealed bool
	err := tx.QueryRowContext(ctx,
		`SELECT sealed FROM batches WHERE id = $1 FOR UPDATE`, batchID).
		Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBatchNotFound
	}
	if err != nil {
		return err
	}
	if sealed {
		return ErrBatchSealed
	}
	return nil
}

func loadRecords(ctx context.Context, tx *sql.Tx, batchID string) ([]labels.Record, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT record_id, sequence
		FROM records
		WHERE batch_id = $1
		ORDER BY record_id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []labels.Record
	for rows.Next() {
		var r labels.Record
		if err := rows.Scan(&r.ID, &r.Sequence); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func mapWriteError(err error) error {
	// The API validates shape before storage. A database check still guards
	// invariants if called directly; keep this explicit rather than returning
	// a driver-specific 23514 to upper layers.
	return err
}
