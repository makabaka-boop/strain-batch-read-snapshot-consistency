// Package labels provides batch sequence validation and shortest unique
// substring certification.
package labels

// Compute returns the shortest unique substring label for each record.
//
// The implementation builds one generalized suffix array over the batch, with
// byte 0 separators that cannot occur in validated a-z sequences. For a suffix
// beginning in record R, the closest suffix-array neighbors owned by a
// different record determine the longest prefix shared with another record.
// A substring one byte longer than that maximum is absent from every other
// record. Each suffix-array rank-range minimum is answered by a segment tree.
// This is O(N log N) time and O(N) memory for batch total length N; it never
// scans candidate fragments against all other full texts.
func Compute(records []Record) (map[string]*Label, error) {
	if err := Validate(records); err != nil {
		return nil, err
	}
	result := make(map[string]*Label, len(records))
	if len(records) == 0 {
		return result, nil
	}

	lengths := make([]int, len(records))
	total := 0
	for i, r := range records {
		lengths[i] = len(r.Sequence)
		total += lengths[i]
	}
	n := total + len(records)
	text := make([]byte, 0, n)
	owner := make([]int32, 0, n)
	offset := make([]int, len(records))
	for i, r := range records {
		offset[i] = len(text)
		text = append(text, r.Sequence...)
		for range r.Sequence {
			owner = append(owner, int32(i))
		}
		text = append(text, 0)
		owner = append(owner, -1)
	}

	sa := buildSuffixArray(text)
	lcp := buildLCP(text, sa)
	rmq := newMinTree(lcp)
	nearLeft, nearRight := nearestDifferentOwners(owner, sa)

	bestLength := make([]int, len(records))
	bestStart := make([]int, len(records))
	for i := range records {
		bestLength[i] = -1
	}
	for rank, globalPos := range sa {
		recordIndex := int(owner[globalPos])
		if recordIndex < 0 {
			continue
		}
		maxShared := 0
		if other := nearLeft[rank]; other >= 0 {
			maxShared = max(maxShared, rmq.rangeLCP(other, rank))
		}
		if other := nearRight[rank]; other >= 0 {
			maxShared = max(maxShared, rmq.rangeLCP(rank, other))
		}

		localStart := globalPos - offset[recordIndex]
		suffixLength := lengths[recordIndex] - localStart
		if maxShared < suffixLength {
			candidateLength := maxShared + 1
			if bestLength[recordIndex] == -1 ||
				candidateLength < bestLength[recordIndex] ||
				(candidateLength == bestLength[recordIndex] && localStart < bestStart[recordIndex]) {
				bestLength[recordIndex] = candidateLength
				bestStart[recordIndex] = localStart
			}
		}
	}

	for i, r := range records {
		if bestLength[i] >= 0 {
			start := bestStart[i]
			end := start + bestLength[i]
			result[r.ID] = &Label{
				Start:     start,
				End:       end,
				Substring: r.Sequence[start:end],
			}
		} else {
			result[r.ID] = nil
		}
	}
	return result, nil
}

// buildSuffixArray uses the doubling algorithm with two linear counting sorts
// per doubling round. It compresses equal rank pairs to classes and is
// O(N log N) total for N suffixes.
func buildSuffixArray(text []byte) []int {
	n := len(text)
	if n == 0 {
		return nil
	}
	sa := make([]int, n)
	tmp := make([]int, n)
	rank := make([]int, n)
	nextRank := make([]int, n)
	cnt := make([]int, max(n+2, 257))

	for i := range sa {
		sa[i] = i
		rank[i] = int(text[i]) + 1
	}
	// Initial character ranks are 1..256; zero is reserved for "past end".
	countingSort(sa, tmp, func(p int) int { return rank[p] }, 256, cnt)
	sa, tmp = tmp, sa

	// Compress initial byte ranks to [1,classCount] so all later counting
	// sorts only need an N-sized scratch array.
	classes := 1
	rank[sa[0]] = 1
	for i := 1; i < n; i++ {
		if text[sa[i]] != text[sa[i-1]] {
			classes++
		}
		rank[sa[i]] = classes
	}

	for width := 1; classes < n; width *= 2 {
		// Stable sort by the second half-rank, then by the first.
		countingSort(sa, tmp, func(p int) int {
			if p+width < n {
				return rank[p+width]
			}
			return 0
		}, n, cnt)
		countingSort(tmp, sa, func(p int) int { return rank[p] }, n, cnt)
		nextClass := 1
		nextRank[sa[0]] = 1
		for i := 1; i < n; i++ {
			prev, cur := sa[i-1], sa[i]
			pr, cr := 0, 0
			if prev+width < n {
				pr = rank[prev+width]
			}
			if cur+width < n {
				cr = rank[cur+width]
			}
			if rank[prev] != rank[cur] || pr != cr {
				nextClass++
			}
			nextRank[cur] = nextClass
		}
		rank, nextRank = nextRank, rank
		classes = nextClass
	}
	return sa
}

// countingSort performs a stable counting sort by integer keys in [0,keyMax].
func countingSort(in, out []int, key func(int) int, keyMax int, cnt []int) {
	for i := 0; i <= keyMax; i++ {
		cnt[i] = 0
	}
	for _, p := range in {
		cnt[key(p)]++
	}
	sum := 0
	for i := 0; i <= keyMax; i++ {
		count := cnt[i]
		cnt[i] = sum
		sum += count
	}
	for _, p := range in {
		k := key(p)
		out[cnt[k]] = p
		cnt[k]++
	}
}

// buildLCP returns Kasai's LCP array: lcp[r] is LCP(sa[r-1], sa[r]); lcp[0]=0.
func buildLCP(text []byte, sa []int) []int {
	n := len(text)
	rankAt := make([]int, n)
	for rank, p := range sa {
		rankAt[p] = rank
	}
	lcp := make([]int, n)
	matched := 0
	for p := 0; p < n; p++ {
		rank := rankAt[p]
		if rank == 0 {
			matched = 0
			continue
		}
		q := sa[rank-1]
		for p+matched < n && q+matched < n && text[p+matched] == text[q+matched] {
			matched++
		}
		lcp[rank] = matched
		if matched > 0 {
			matched--
		}
	}
	return lcp
}

// nearestDifferentOwners returns, for every suffix rank, the nearest rank on
// each side whose suffix starts in another record. Separator suffixes are
// ignored. It compacts non-separator ranks into nodes, then applies the
// recurrence "if the adjacent node has a different owner it is the answer;
// otherwise reuse that node's already-skipped nearest different owner",
// making both directions linear.
func nearestDifferentOwners(owner []int32, sa []int) ([]int, []int) {
	n := len(sa)
	left := make([]int, n)
	right := make([]int, n)
	for i := range left {
		left[i], right[i] = -1, -1
	}

	nodes := make([]int, 0, n)
	nodeOwner := make([]int, 0, n)
	for rank, p := range sa {
		if o := int(owner[p]); o >= 0 {
			nodes = append(nodes, rank)
			nodeOwner = append(nodeOwner, o)
		}
	}

	prevDifferent := make([]int, len(nodes))
	for i := range nodes {
		switch {
		case i == 0:
			prevDifferent[i] = -1
		case nodeOwner[i-1] != nodeOwner[i]:
			prevDifferent[i] = i - 1
		default:
			prevDifferent[i] = prevDifferent[i-1]
		}
		if j := prevDifferent[i]; j >= 0 {
			left[nodes[i]] = nodes[j]
		}
	}

	nextDifferent := make([]int, len(nodes))
	for i := len(nodes) - 1; i >= 0; i-- {
		switch {
		case i == len(nodes)-1:
			nextDifferent[i] = -1
		case nodeOwner[i+1] != nodeOwner[i]:
			nextDifferent[i] = i + 1
		default:
			nextDifferent[i] = nextDifferent[i+1]
		}
		if j := nextDifferent[i]; j >= 0 {
			right[nodes[i]] = nodes[j]
		}
	}
	return left, right
}

// minTree is a segment tree over LCP array values.
type minTree struct {
	size int
	tree []int
}

func newMinTree(values []int) *minTree {
	size := 1
	for size < len(values) {
		size *= 2
	}
	tree := make([]int, 2*size)
	inf := len(values) + 1
	for i := range tree {
		tree[i] = inf
	}
	copy(tree[size:], values)
	for i := size - 1; i > 0; i-- {
		tree[i] = min(tree[i*2], tree[i*2+1])
	}
	return &minTree{size: size, tree: tree}
}

// rangeLCP returns min(lcp[leftRank+1:rightRank]) where leftRank < rightRank.
func (t *minTree) rangeLCP(leftRank, rightRank int) int {
	l := leftRank + 1 + t.size
	r := rightRank + t.size
	result := t.tree[0]
	for l <= r {
		if l%2 == 1 {
			result = min(result, t.tree[l])
			l++
		}
		if r%2 == 0 {
			result = min(result, t.tree[r])
			r--
		}
		l /= 2
		r /= 2
	}
	return result
}
