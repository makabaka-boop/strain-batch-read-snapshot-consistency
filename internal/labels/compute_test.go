package labels

import (
	"strings"
	"testing"
)

func TestSpecExamples(t *testing.T) {
	records := []Record{
		{ID: "a", Sequence: "ababa"},
		{ID: "b", Sequence: "babab"},
		{ID: "c", Sequence: "zzaba"},
	}
	got, err := Compute(records)
	if err != nil {
		t.Fatalf("Compute returned error: %v", err)
	}
	assertLabel(t, got, "a", "ababa")
	assertLabel(t, got, "b", "babab")
	assertLabel(t, got, "c", "z")
	if got["c"].Start != 0 {
		t.Fatalf("c start = %d, want 0", got["c"].Start)
	}
}

func TestIdenticalSequencesHaveNoLabels(t *testing.T) {
	got, err := Compute([]Record{
		{ID: "a", Sequence: "abc"},
		{ID: "b", Sequence: "abc"},
	})
	if err != nil {
		t.Fatalf("Compute returned error: %v", err)
	}
	if got["a"] != nil || got["b"] != nil {
		t.Fatalf("identical records labels = %#v, want both nil", got)
	}
}

func TestSingleRecordUsesShortestEarliest(t *testing.T) {
	got, err := Compute([]Record{{ID: "only", Sequence: "banana"}})
	if err != nil {
		t.Fatalf("Compute returned error: %v", err)
	}
	// All one-character strings repeat inside this record, but repetition in
	// the same record is allowed. The earliest length-one character is b.
	assertLabel(t, got, "only", "b")
}

func TestNaiveOracleSmallInputs(t *testing.T) {
	tests := [][]Record{
		{},
		{{ID: "x", Sequence: "a"}},
		{
			{ID: "a", Sequence: "abc"},
			{ID: "b", Sequence: "bcd"},
			{ID: "c", Sequence: "x"},
		},
		{
			{ID: "a", Sequence: "aaaa"},
			{ID: "b", Sequence: "aaab"},
			{ID: "c", Sequence: "baaa"},
		},
		{
			{ID: "same1", Sequence: "abab"},
			{ID: "same2", Sequence: "abab"},
			{ID: "other", Sequence: "abac"},
		},
		{
			{ID: "a", Sequence: "zzz"},
			{ID: "b", Sequence: "zzz"},
			{ID: "c", Sequence: "zz"},
		},
		{
			{ID: "a", Sequence: "abcabc"},
			{ID: "b", Sequence: "bcabca"},
			{ID: "c", Sequence: "cabcaab"},
		},
	}
	for _, records := range tests {
		want := Naive(records)
		got, err := Compute(records)
		if err != nil {
			t.Fatalf("Compute(%v) returned error: %v", records, err)
		}
		for _, r := range records {
			w, g := want[r.ID], got[r.ID]
			if (w == nil) != (g == nil) {
				t.Fatalf("record %s nil mismatch: got %v want %v", r.ID, g, w)
			}
			if w == nil {
				continue
			}
			if g.Start != w.Start || g.End != w.End || g.Substring != w.Substring {
				t.Fatalf("record %s = %+v, want %+v", r.ID, g, w)
			}
		}
	}
}

func TestRandomSmallInputsAgainstNaiveOracle(t *testing.T) {
	// Deterministic, broad overlap/duplicate coverage without introducing an
	// external randomness dependency into runtime code.
	seed := uint32(0x12345678)
	next := func() uint32 {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		return seed
	}
	alphabet := "abc"
	for trial := 0; trial < 200; trial++ {
		recordCount := 1 + int(next()%5)
		records := make([]Record, recordCount)
		for i := range records {
			n := 1 + int(next()%12)
			buf := make([]byte, n)
			for j := range buf {
				buf[j] = alphabet[next()%uint32(len(alphabet))]
			}
			records[i] = Record{ID: string(rune('a' + i)), Sequence: string(buf)}
		}
		got, err := Compute(records)
		if err != nil {
			t.Fatalf("trial %d Compute: %v", trial, err)
		}
		want := Naive(records)
		for _, r := range records {
			w, g := want[r.ID], got[r.ID]
			if (w == nil) != (g == nil) {
				t.Fatalf("trial %d records=%v record %s mismatch: got %v want %v",
					trial, records, r.ID, g, w)
			}
			if w != nil && (g.Start != w.Start || g.End != w.End || g.Substring != w.Substring) {
				t.Fatalf("trial %d records=%v record %s = %+v, want %+v",
					trial, records, r.ID, g, w)
			}
		}
	}
}

// Naive is the deliberately simple reference implementation. For every start,
// it searches increasing lengths by checking the candidate against every other
// full text. It is used only in tests as an independent oracle.
func Naive(records []Record) map[string]*Label {
	result := make(map[string]*Label, len(records))
	for i, r := range records {
		var best *Label
		for start := 0; start < len(r.Sequence); start++ {
			for end := start + 1; end <= len(r.Sequence); end++ {
				fragment := r.Sequence[start:end]
				unique := true
				for j, other := range records {
					if i == j {
						continue
					}
					if strings.Contains(other.Sequence, fragment) {
						unique = false
						break
					}
				}
				if unique {
					if best == nil || end-start < best.End-best.Start ||
						(end-start == best.End-best.Start && start < best.Start) {
						best = &Label{Start: start, End: end, Substring: fragment}
					}
					break
				}
			}
		}
		result[r.ID] = best
	}
	return result
}

func assertLabel(t *testing.T, labels map[string]*Label, id, substring string) {
	t.Helper()
	got := labels[id]
	if got == nil {
		t.Fatalf("label for %s is null, want %q", id, substring)
	}
	if got.Substring != substring {
		t.Fatalf("label for %s = %q, want %q", id, got.Substring, substring)
	}
	if got.Start < 0 || got.End <= got.Start {
		t.Fatalf("label for %s has invalid range [%d,%d)", id, got.Start, got.End)
	}
	if r := []rune(got.Substring); len(r) != got.End-got.Start {
		t.Fatalf("label for %s length mismatch", id)
	}
}
