package labels

import "testing"

func TestOwnerNearestInterleavingRegression(t *testing.T) {
	// Ranks after separator removal have owners A B A C A. For every A, the
	// nearest different owner is B or C rather than a stale "second last" A.
	owner := []int32{0, 1, 0, 2, 0}
	sa := []int{0, 1, 2, 3, 4}
	left, right := nearestDifferentOwners(owner, sa)
	if left[4] != 3 || right[4] != -1 {
		t.Fatalf("rank 4 left/right = %d/%d, want 3/-1", left[4], right[4])
	}
	if left[2] != 1 || right[2] != 3 {
		t.Fatalf("rank 2 left/right = %d/%d, want 1/3", left[2], right[2])
	}
	if left[0] != -1 || right[0] != 1 {
		t.Fatalf("rank 0 left/right = %d/%d, want -1/1", left[0], right[0])
	}
}
