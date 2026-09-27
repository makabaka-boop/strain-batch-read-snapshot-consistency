package storage_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"seqlabels/internal/api"
	"seqlabels/internal/labels"
	"seqlabels/internal/storage"

	_ "github.com/lib/pq"
)

var schemaCounter uint64

func TestDraftSealReportAndRestartCertification(t *testing.T) {
	store, schemaName := newIsolatedStore(t)
	server := httptest.NewServer(api.NewServer(store, labels.Compute).Handler())
	defer server.Close()
	client := server.Client()

	// Create an empty draft through HTTP.
	resp, err := client.Post(server.URL+"/batches", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var batch struct {
		ID      string `json:"id"`
		Sealed  bool   `json:"sealed"`
		Records []any  `json:"records"`
	}
	decodeBody(t, resp, &batch)
	if len(batch.Records) != 0 {
		t.Fatalf("new batch records = %v, want empty", batch.Records)
	}

	// Invalid writes must leave the draft unchanged.
	invalidRequests := []string{
		`{"sequence":""}`,
		`{"sequence":"ABC"}`,
		`{"sequence":"a b"}`,
		`{"unknown":"abc"}`,
	}
	for _, body := range invalidRequests {
		req, _ := http.NewRequest(http.MethodPut,
			server.URL+"/batches/"+batch.ID+"/records/bad", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("invalid request body %q status = %d", body, resp.StatusCode)
		}
	}

	// Valid maintenance calls.
	putRecord(t, client, server.URL, batch.ID, "a", "ababa")
	putRecord(t, client, server.URL, batch.ID, "b", "babab")
	putRecord(t, client, server.URL, batch.ID, "c", "zzaba")

	gotBatch := getJSON(t, client, server.URL+"/batches/"+batch.ID)
	records := recordsFromResponse(t, gotBatch)
	if len(records) != 3 {
		t.Fatalf("record count = %d, want 3", len(records))
	}

	// Seal creates the immutable certification report.
	resp, err = client.Post(server.URL+"/batches/"+batch.ID+"/seal", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seal status = %d", resp.StatusCode)
	}
	var report struct {
		ID        string                   `json:"id"`
		BatchID   string                   `json:"batchId"`
		Algorithm string                   `json:"algorithm"`
		InputHash string                   `json:"inputHash"`
		Records   []labels.Record          `json:"records"`
		Labels    map[string]*labels.Label `json:"labels"`
	}
	decodeBody(t, resp, &report)
	if report.Algorithm != labels.AlgorithmVersion {
		t.Fatalf("algorithm = %q", report.Algorithm)
	}
	if report.Labels["a"] == nil || report.Labels["a"].Substring != "ababa" {
		t.Fatalf("label a = %#v", report.Labels["a"])
	}
	if report.Labels["b"] == nil || report.Labels["b"].Substring != "babab" {
		t.Fatalf("label b = %#v", report.Labels["b"])
	}
	if report.Labels["c"] == nil || report.Labels["c"].Substring != "z" || report.Labels["c"].Start != 0 {
		t.Fatalf("label c = %#v", report.Labels["c"])
	}

	// Any later draft mutation is rejected and cannot alter the frozen input.
	req, _ := http.NewRequest(http.MethodPut,
		server.URL+"/batches/"+batch.ID+"/records/a", strings.NewReader(`{"sequence":"zzzzz"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post-seal PUT status = %d, want 409", resp.StatusCode)
	}

	// Simulate an API/container restart with a fresh connection pool while the
	// database volume remains available.
	restarted, err := openIsolated(schemaName)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedReport, err := storage.NewStore(restarted).GetReport(context.Background(), batch.ID)
	if err != nil {
		t.Fatalf("read report after restart: %v", err)
	}
	if len(restartedReport.Records) != len(report.Records) {
		t.Fatalf("restarted record count = %d", len(restartedReport.Records))
	}
	for i := range report.Records {
		if restartedReport.Records[i] != report.Records[i] {
			t.Fatalf("restarted record %d = %+v, want %+v", i, restartedReport.Records[i], report.Records[i])
		}
	}

	// Independently recalculate from the exact frozen input and verify every
	// returned label, including null results, byte-for-byte.
	independentHash, err := labels.CanonicalHash(restartedReport.Records)
	if err != nil {
		t.Fatal(err)
	}
	if independentHash != restartedReport.InputHash {
		t.Fatalf("input hash changed: %q vs %q", independentHash, restartedReport.InputHash)
	}
	independentLabels, err := labels.Compute(restartedReport.Records)
	if err != nil {
		t.Fatal(err)
	}
	if len(independentLabels) != len(restartedReport.Labels) {
		t.Fatalf("label map size after restart = %d", len(restartedReport.Labels))
	}
	for id, want := range independentLabels {
		got := restartedReport.Labels[id]
		if (want == nil) != (got == nil) {
			t.Fatalf("id %s null mismatch", id)
		}
		if want != nil && *want != *got {
			t.Fatalf("id %s label = %+v, want %+v", id, got, want)
		}
	}

	// Report remains available at its read endpoint after restart.
	restartedAPI := httptest.NewServer(api.NewServer(storage.NewStore(restarted), labels.Compute).Handler())
	defer restartedAPI.Close()
	var reread struct {
		InputHash string `json:"inputHash"`
	}
	resp2, err := client.Get(restartedAPI.URL + "/batches/" + batch.ID + "/report")
	if err != nil {
		t.Fatal(err)
	}
	decodeBody(t, resp2, &reread)
	resp2.Body.Close()
	if reread.InputHash != report.InputHash {
		t.Fatalf("reread hash = %v, want %s", reread.InputHash, report.InputHash)
	}
}

func TestInvalidBatchSizeLeavesDraftIntact(t *testing.T) {
	store, _ := newIsolatedStore(t)
	ctx := context.Background()
	batchID, _, err := store.CreateBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("a", labels.MaxSequenceLen)
	second := strings.Repeat("b", labels.MaxSequenceLen)
	if err := store.PutRecord(ctx, batchID, "a", first); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRecord(ctx, batchID, "b", second); err != nil {
		t.Fatal(err)
	}
	original, err := store.GetBatch(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}

	// 200,000 new bytes would bring the total to 600,000.
	tooLarge := strings.Repeat("c", labels.MaxSequenceLen)
	if err := store.PutRecord(ctx, batchID, "c", tooLarge); err != labels.ErrBatchTooBig {
		t.Fatalf("PutRecord error = %v, want ErrBatchTooBig", err)
	}
	afterRejected, err := store.GetBatch(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRejected.Records) != len(original.Records) {
		t.Fatalf("rejected write changed records: %d vs %d", len(afterRejected.Records), len(original.Records))
	}

	// Replacing b remains legal at exactly 400,000 total.
	if err := store.PutRecord(ctx, batchID, "b", strings.Repeat("d", labels.MaxSequenceLen)); err != nil {
		t.Fatalf("legal replacement failed: %v", err)
	}
}

func TestEmptyBatchCanSealAndReport(t *testing.T) {
	store, _ := newIsolatedStore(t)
	ctx := context.Background()
	id, _, err := store.CreateBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.Freeze(ctx, id, labels.Compute)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Records) != 0 || len(report.Labels) != 0 {
		t.Fatalf("empty report = %+v", report)
	}
	if _, err := store.Freeze(ctx, id, labels.Compute); err != storage.ErrBatchSealed {
		t.Fatalf("second Freeze error = %v, want ErrBatchSealed", err)
	}
}

func TestComputeFailureLeavesEditableDraftAndNoReport(t *testing.T) {
	store, _ := newIsolatedStore(t)
	ctx := context.Background()
	id, _, err := store.CreateBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRecord(ctx, id, "a", "abc"); err != nil {
		t.Fatal(err)
	}
	computeErr := errors.New("simulated certification failure")
	if _, err := store.Freeze(ctx, id, func([]labels.Record) (map[string]*labels.Label, error) {
		return nil, computeErr
	}); !errors.Is(err, computeErr) {
		t.Fatalf("Freeze error = %v, want %v", err, computeErr)
	}
	if _, err := store.GetReport(ctx, id); err != storage.ErrReportNotFound {
		t.Fatalf("report after failed seal = %v, want ErrReportNotFound", err)
	}
	batch, err := store.GetBatch(ctx, id)
	if err != nil {
		t.Fatalf("read draft after failed seal: %v", err)
	}
	if batch.Sealed || len(batch.Records) != 1 || batch.Records[0].Sequence != "abc" {
		t.Fatalf("draft after failed seal = %+v", batch)
	}
	if err := store.PutRecord(ctx, id, "a", "abcd"); err != nil {
		t.Fatalf("draft should remain editable: %v", err)
	}
}

func TestGetBatchUsesOneSnapshotAcrossBatchAndRecords(t *testing.T) {
	store, _ := newIsolatedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	id, _, err := store.CreateBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRecord(ctx, id, "a", "aaa"); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetBatch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// Hold an ACCESS EXCLUSIVE table lock after making the draft changes but
	// before committing. A GetBatch caller can therefore read batches first and
	// become blocked on records at the exact boundary between its two queries.
	writer, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `LOCK TABLE records IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx,
		`UPDATE records SET sequence = 'bbb' WHERE batch_id = $1 AND record_id = 'a'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx,
		`UPDATE batches SET sealed = true, updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	type result struct {
		batch *storage.Batch
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		batch, err := store.GetBatch(ctx, id)
		resultCh <- result{batch: batch, err: err}
	}()
	if !waitForQueryLock(t, ctx, store, `%FROM records%`) {
		t.Fatal("GetBatch did not reach the locked records query")
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("GetBatch after concurrent commit: %v", got.err)
		}
		if got.batch.Sealed {
			t.Fatal("read mixed states: sealed flag came from the new snapshot")
		}
		if !got.batch.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("updated_at = %s, want snapshot value %s", got.batch.UpdatedAt, before.UpdatedAt)
		}
		if len(got.batch.Records) != 1 || got.batch.Records[0].Sequence != "aaa" {
			t.Fatalf("records = %+v, want snapshot record aaa", got.batch.Records)
		}
	case <-ctx.Done():
		t.Fatalf("GetBatch did not return after writer commit: %v", ctx.Err())
	}
}

func waitForQueryLock(t *testing.T, ctx context.Context, store *storage.Store, queryPattern string) bool {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		err := store.DB().QueryRowContext(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND query LIKE $1`, queryPattern).Scan(&waiting)
		if err != nil {
			t.Fatalf("check blocked query: %v", err)
		}
		if waiting > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func newIsolatedStore(t *testing.T) (*storage.Store, string) {
	t.Helper()
	db, schemaName := openTestDatabase(t)
	store := storage.NewStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		t.Fatalf("migrate schema %s: %v", schemaName, err)
	}
	t.Cleanup(func() {
		db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schemaName+` CASCADE`)
		db.Close()
	})
	return store, schemaName
}

func openTestDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		baseURL = "postgres://seqlabels:seqlabels@localhost:5432/seqlabels?sslmode=disable"
	}
	schemaName := fmt.Sprintf("it_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&schemaCounter, 1))
	return mustOpenIsolated(t, baseURL, schemaName)
}

func openIsolated(schemaName string) (*sql.DB, error) {
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		baseURL = "postgres://seqlabels:seqlabels@localhost:5432/seqlabels?sslmode=disable"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	return sql.Open("postgres", u.String())
}

func mustOpenIsolated(t *testing.T, baseURL, schemaName string) (*sql.DB, string) {
	t.Helper()
	admin, err := sql.Open("postgres", baseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if err := admin.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("connect real PostgreSQL at %s: %v", baseURL, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schemaName); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	admin.Close()
	db, err := openIsolated(schemaName)
	if err != nil {
		t.Fatal(err)
	}
	return db, schemaName
}

func putRecord(t *testing.T, client *http.Client, baseURL, batchID, id, seq string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"sequence": seq})
	req, _ := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/batches/%s/records/%s", baseURL, batchID, id), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT %s status = %d", id, resp.StatusCode)
	}
}

func getJSON(t *testing.T, client *http.Client, target string) json.RawMessage {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", target, resp.StatusCode)
	}
	var raw json.RawMessage
	decodeBody(t, resp, &raw)
	return raw
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func recordsFromResponse(t *testing.T, raw json.RawMessage) []labels.Record {
	t.Helper()
	var wrapper struct {
		Records []labels.Record `json:"records"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatal(err)
	}
	return wrapper.Records
}
