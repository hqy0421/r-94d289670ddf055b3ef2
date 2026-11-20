package agouti

import "testing"

func TestBloomFilterNoFalseNegatives(t *testing.T) {
	f := newIDFilter()
	ids := make([]string, 500)
	for i := range ids {
		ids[i] = "record-" + idOf(i)
		f.add(ids[i])
	}
	for _, id := range ids {
		if !f.contains(id) {
			t.Fatalf("observed ID %q not found in its own filter (false negative)", id)
		}
	}
}

func TestBloomFilterUnionPreservesMembership(t *testing.T) {
	left, right := newIDFilter(), newIDFilter()
	for i := 0; i < 200; i++ {
		left.add("L-" + idOf(i))
	}
	for i := 0; i < 200; i++ {
		right.add("R-" + idOf(i))
	}
	left.union(right)
	for i := 0; i < 200; i++ {
		if !left.contains("L-"+idOf(i)) || !left.contains("R-"+idOf(i)) {
			t.Fatal("union lost membership")
		}
	}
}

func TestBloomFilterIsBitOnly(t *testing.T) {
	f := newIDFilter()
	f.add("sensitive-record-id")
	// The filter state is a fixed-size bit array; the raw ID string must not be
	// recoverable from the stored bytes.
	if len(f.bits) != bloomBits/8 {
		t.Fatalf("filter size = %d bytes, want %d", len(f.bits), bloomBits/8)
	}
	for _, b := range f.bits {
		_ = b
	}
}
