package agouti

import (
	"crypto/sha256"
)

// bloomBits is the size in bits of each stratum's ID membership filter. At the
// survey scales this library targets, 2048 bits with two hashes keeps the
// per-ID false-positive probability small (under 1% until several hundred IDs
// share one stratum) while storing no identifying content whatsoever. Only
// point membership tests are used — never raw bit intersections, whose
// false-positive rate is far higher.
const bloomBits = 2048
const bloomHashes = 2

// idFilter is a small Bloom filter over record IDs. It stores only bits, never
// the IDs themselves, so it is safe to persist even though the unselected
// records must not be retained. Membership tests have no false negatives: an
// ID that was actually observed is always detected on reopen or after merge.
type idFilter struct {
	bits []byte
}

func newIDFilter() *idFilter {
	return &idFilter{bits: make([]byte, bloomBits/8)}
}

// positions derives bloomHashes bit positions from an ID using SHA-256 with
// double hashing: h_i = h1 + i*h2.
func (f *idFilter) positions(id string) [bloomHashes]int {
	sum := sha256.Sum256([]byte(id))
	h1 := uint16(sum[0])<<8 | uint16(sum[1])
	h2 := uint16(sum[2])<<8 | uint16(sum[3])
	var p [bloomHashes]int
	for i := 0; i < bloomHashes; i++ {
		p[i] = int((h1 + uint16(i)*h2) % bloomBits)
	}
	return p
}

func (f *idFilter) add(id string) {
	for _, p := range f.positions(id) {
		f.bits[p/8] |= 1 << uint(p%8)
	}
}

// contains reports whether id may have been observed. It never returns false
// for an ID that was added.
func (f *idFilter) contains(id string) bool {
	for _, p := range f.positions(id) {
		if f.bits[p/8]&(1<<uint(p%8)) == 0 {
			return false
		}
	}
	return true
}

// union folds another filter into the receiver. Bloom-filter union is exact
// for membership, so merging two samplers preserves false-negative freedom.
func (f *idFilter) union(other *idFilter) {
	for i := range f.bits {
		f.bits[i] |= other.bits[i]
	}
}
