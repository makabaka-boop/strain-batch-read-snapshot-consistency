package labels

import "testing"

func BenchmarkComputeHighOverlapMaximumBatch(b *testing.B) {
	const recordCount = 5
	base := make([]byte, MaxBatchLen)
	for i := range base {
		base[i] = byte('a' + i%4)
	}
	records := make([]Record, recordCount)
	each := MaxBatchLen / recordCount
	for i := range records {
		start := i % 4
		end := start + each
		// Deliberately share almost all content; each record differs only at
		// one sparse location, making many fragments public.
		seq := append([]byte(nil), base[start:end]...)
		seq[len(seq)/2+i] = byte('y')
		records[i] = Record{ID: string(rune('a' + i)), Sequence: string(seq)}
	}
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		if _, err := Compute(records); err != nil {
			b.Fatal(err)
		}
	}
}
