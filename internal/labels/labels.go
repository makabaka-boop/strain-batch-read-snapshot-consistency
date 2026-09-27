// Package labels validates sequence records and computes the shortest
// record-unique substring for every record in a batch.
package labels

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const (
	// MaxIDLen is the maximum ASCII identifier length.
	MaxIDLen = 128
	// MaxSequenceLen is the maximum length of one sequence.
	MaxSequenceLen = 200_000
	// MaxBatchLen is the maximum total length of all draft sequences.
	MaxBatchLen = 500_000
	// AlgorithmVersion identifies the certified output format and rule.
	AlgorithmVersion = "shortest-unique-substring/v1"
)

var (
	ErrInvalidBatch   = errors.New("invalid batch records")
	ErrDuplicateID    = errors.New("duplicate record id")
	ErrInvalidID      = errors.New("invalid record id")
	ErrInvalidSeq     = errors.New("invalid sequence")
	ErrSequenceTooBig = errors.New("sequence too long")
	ErrBatchTooBig    = errors.New("batch total length too large")
)

// Record is one frozen or draft input record.
type Record struct {
	ID       string `json:"id"`
	Sequence string `json:"sequence"`
}

// Label is the selected substring. A nil *Label means no substring is unique
// to that record among all records in the batch.
type Label struct {
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Substring string `json:"substring"`
}

// Validate normalizes nothing: identifiers and sequences must already match
// their exact canonical representation.
func Validate(records []Record) error {
	seen := make(map[string]struct{}, len(records))
	total := 0
	for _, r := range records {
		if err := ValidateID(r.ID); err != nil {
			return err
		}
		if _, ok := seen[r.ID]; ok {
			return ErrDuplicateID
		}
		seen[r.ID] = struct{}{}
		if r.Sequence == "" {
			return ErrInvalidSeq
		}
		if len(r.Sequence) > MaxSequenceLen {
			return ErrSequenceTooBig
		}
		for i := 0; i < len(r.Sequence); i++ {
			c := r.Sequence[i]
			if c < 'a' || c > 'z' {
				return ErrInvalidSeq
			}
		}
		total += len(r.Sequence)
		if total > MaxBatchLen {
			return ErrBatchTooBig
		}
	}
	return nil
}

// ValidateID accepts non-empty ASCII strings containing letters, digits,
// underscore and hyphen. IDs are compared byte-for-byte.
func ValidateID(id string) error {
	if id == "" || len(id) > MaxIDLen {
		return ErrInvalidID
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 0x80 {
			return ErrInvalidID
		}
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return ErrInvalidID
		}
	}
	return nil
}

// CanonicalHash hashes records in ID order. It covers exactly the certified
// input, so the same report can be independently reproduced and compared.
func CanonicalHash(records []Record) (string, error) {
	sorted := append([]Record(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	data, err := json.Marshal(struct {
		Algorithm string   `json:"algorithm"`
		Records   []Record `json:"records"`
	}{Algorithm: AlgorithmVersion, Records: sorted})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
