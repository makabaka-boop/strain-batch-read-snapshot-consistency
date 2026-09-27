// Package api implements the versionless HTTP JSON API.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"seqlabels/internal/labels"
	"seqlabels/internal/storage"
)

// ComputeFunc computes labels from one batch's frozen record set.
type ComputeFunc func(records []labels.Record) (map[string]*labels.Label, error)

// Server holds HTTP dependencies.
type Server struct {
	store   *storage.Store
	compute ComputeFunc
	mux     *http.ServeMux
}

// NewServer wires routes and dependencies.
func NewServer(store *storage.Store, compute ComputeFunc) *Server {
	s := &Server{store: store, compute: compute}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/batches", s.createBatch)
	mux.HandleFunc("/batches/", s.batchRoutes)
	s.mux = mux
	return s
}

// Handler returns the complete HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type batchResponse struct {
	ID        string           `json:"id"`
	Sealed    bool             `json:"sealed"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
	Records   []recordResponse `json:"records"`
}

type recordResponse struct {
	ID       string `json:"id"`
	Sequence string `json:"sequence"`
}

type putRecordRequest struct {
	Sequence *string `json:"sequence"`
}

type reportResponse struct {
	ID        string                   `json:"id"`
	BatchID   string                   `json:"batchId"`
	Algorithm string                   `json:"algorithm"`
	InputHash string                   `json:"inputHash"`
	SealedAt  time.Time                `json:"sealedAt"`
	Records   []labels.Record          `json:"records"`
	Labels    map[string]*labels.Label `json:"labels"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET is required")
		return
	}
	if err := s.store.DB().PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "service is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST is required")
		return
	}
	if r.Body != nil {
		defer r.Body.Close()
		// The endpoint takes no fields. Consume a small body so an empty
		// object is accepted but trailing JSON cannot be introduced later
		// as an accidental backward-compatible input.
		var body map[string]json.RawMessage
		if err := decodeJSON(w, r, &body, true); err != nil {
			writeRequestError(w, err)
			return
		}
		if len(body) != 0 {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body must be empty")
			return
		}
	}
	id, createdAt, err := s.store.CreateBatch(r.Context())
	if err != nil {
		writeStorageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        id,
		"sealed":    false,
		"createdAt": createdAt,
		"records":   []recordResponse{},
	})
}

func (s *Server) batchRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/batches/")
	parts := strings.Split(path, "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		s.getBatch(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "seal":
		s.sealBatch(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "report":
		s.getReport(w, r, parts[0])
	case len(parts) == 3 && parts[1] == "records" && parts[2] != "":
		s.recordRoute(w, r, parts[0], parts[2])
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown endpoint")
	}
}

func (s *Server) getBatch(w http.ResponseWriter, r *http.Request, batchID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET is required")
		return
	}
	if !validUUID(batchID) {
		writeError(w, http.StatusNotFound, "BATCH_NOT_FOUND", "batch not found")
		return
	}
	b, err := s.store.GetBatch(r.Context(), batchID)
	if err != nil {
		writeStorageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBatchResponse(b))
}

func (s *Server) recordRoute(w http.ResponseWriter, r *http.Request, batchID, recordID string) {
	if err := labels.ValidateID(recordID); err != nil {
		writeError(w, http.StatusBadRequest, validationCode(err), "invalid record id")
		return
	}
	if !validUUID(batchID) {
		writeError(w, http.StatusNotFound, "BATCH_NOT_FOUND", "batch not found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.putRecord(w, r, batchID, recordID)
	case http.MethodDelete:
		s.deleteRecord(w, r, batchID, recordID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "PUT or DELETE is required")
	}
}

func (s *Server) putRecord(w http.ResponseWriter, r *http.Request, batchID, recordID string) {
	var req putRecordRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeRequestError(w, err)
		return
	}
	if req.Sequence == nil {
		writeError(w, http.StatusBadRequest, "INVALID_SEQUENCE", "sequence is required")
		return
	}
	if err := validateSequence(*req.Sequence); err != nil {
		writeError(w, http.StatusBadRequest, validationCode(err), err.Error())
		return
	}
	if err := s.store.PutRecord(r.Context(), batchID, recordID, *req.Sequence); err != nil {
		writeStorageError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRecord(w http.ResponseWriter, r *http.Request, batchID, recordID string) {
	if err := s.store.DeleteRecord(r.Context(), batchID, recordID); err != nil {
		writeStorageError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sealBatch(w http.ResponseWriter, r *http.Request, batchID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST is required")
		return
	}
	if !validUUID(batchID) {
		writeError(w, http.StatusNotFound, "BATCH_NOT_FOUND", "batch not found")
		return
	}
	report, err := s.store.Freeze(r.Context(), batchID, s.compute)
	if err != nil {
		writeStorageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toReportResponse(report))
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request, batchID string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET is required")
		return
	}
	if !validUUID(batchID) {
		writeError(w, http.StatusNotFound, "REPORT_NOT_FOUND", "report not found")
		return
	}
	report, err := s.store.GetReport(r.Context(), batchID)
	if err != nil {
		writeStorageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toReportResponse(report))
}

func validateSequence(seq string) error {
	if seq == "" {
		return labels.ErrInvalidSeq
	}
	if len(seq) > labels.MaxSequenceLen {
		return labels.ErrSequenceTooBig
	}
	for i := 0; i < len(seq); i++ {
		c := seq[i]
		if c < 'a' || c > 'z' {
			return labels.ErrInvalidSeq
		}
	}
	return nil
}

func validationCode(err error) string {
	switch {
	case errors.Is(err, labels.ErrDuplicateID):
		return "DUPLICATE_RECORD_ID"
	case errors.Is(err, labels.ErrSequenceTooBig):
		return "SEQUENCE_TOO_LONG"
	case errors.Is(err, labels.ErrBatchTooBig):
		return "BATCH_TOO_LARGE"
	case errors.Is(err, labels.ErrInvalidSeq):
		return "INVALID_SEQUENCE"
	case errors.Is(err, labels.ErrInvalidID), errors.Is(err, labels.ErrInvalidBatch):
		return "INVALID_RECORD_ID"
	default:
		return "INVALID_REQUEST"
	}
}

func writeRequestError(w http.ResponseWriter, err error) {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.Is(err, io.EOF):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is required")
	case errors.As(err, &syntaxErr), errors.As(err, &typeErr):
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "request body must be valid JSON")
	default:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
	}
}

func writeStorageError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrBatchNotFound):
		writeError(w, http.StatusNotFound, "BATCH_NOT_FOUND", "batch not found")
	case errors.Is(err, storage.ErrReportNotFound):
		writeError(w, http.StatusNotFound, "REPORT_NOT_FOUND", "report not found")
	case errors.Is(err, storage.ErrBatchSealed):
		writeError(w, http.StatusConflict, "BATCH_SEALED", "batch is sealed and cannot be modified")
	case errors.Is(err, labels.ErrBatchTooBig):
		writeError(w, http.StatusUnprocessableEntity, "BATCH_TOO_LARGE", "total sequence length must not exceed 500000")
	case errors.Is(err, labels.ErrSequenceTooBig):
		writeError(w, http.StatusUnprocessableEntity, "SEQUENCE_TOO_LONG", "sequence must not exceed 200000 bytes")
	default:
		// Log via server logging would go here; the wire response keeps a
		// stable code and never emits driver internals.
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) error {
	if r.Body != nil {
		defer r.Body.Close()
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func toBatchResponse(b *storage.Batch) batchResponse {
	resp := batchResponse{
		ID:        b.ID,
		Sealed:    b.Sealed,
		CreatedAt: b.CreatedAt,
		UpdatedAt: b.UpdatedAt,
		Records:   make([]recordResponse, 0, len(b.Records)),
	}
	for _, r := range b.Records {
		resp.Records = append(resp.Records, recordResponse{ID: r.ID, Sequence: r.Sequence})
	}
	return resp
}

func toReportResponse(r *storage.Report) reportResponse {
	return reportResponse{
		ID:        r.ID,
		BatchID:   r.BatchID,
		Algorithm: r.Algorithm,
		InputHash: r.InputHash,
		SealedAt:  r.SealedAt,
		Records:   r.Records,
		Labels:    r.Labels,
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	resp := errorResponse{}
	resp.Error.Code = code
	resp.Error.Message = message
	writeJSON(w, status, resp)
}
