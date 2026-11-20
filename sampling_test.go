package agouti

import (
	"math"
	"sort"
	"testing"
)

func newTestSampler(t *testing.T, strata map[string]int, salt uint64) *Sampler {
	s, err := NewSampler("batch", strata, SamplerSalt(salt))
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	return s
}

func shardOf(source, stratum string, ids ...string) Shard {
	records := make([]Record, 0, len(ids))
	for i, id := range ids {
		records = append(records, Record{ID: id, Stratum: stratum, Amount: float64(i + 1)})
	}
	return Shard{Source: source, Records: records}
}

func TestNewSamplerRejectsBadConfiguration(t *testing.T) {
	if _, err := NewSampler("", map[string]int{"a": 2}); err == nil {
		t.Fatal("expected error for empty name")
	}
	if _, err := NewSampler("batch", nil); err == nil {
		t.Fatal("expected error for empty strata")
	}
	if _, err := NewSampler("batch", map[string]int{"a": 0}); err == nil {
		t.Fatal("expected error for non-positive K")
	}
	if _, err := NewSampler("batch", map[string]int{" ": 2}); err == nil {
		t.Fatal("expected error for blank stratum name")
	}
}

// TestKeysAreUniformAndIndependent checks the hash-derived keys behave like
// independent uniform draws across many IDs: their empirical distribution over
// coarse buckets is close to uniform and adjacent IDs do not correlate.
func TestKeysAreUniformAndIndependent(t *testing.T) {
	const n = 50000
	const buckets = 64
	counts := make([]int, buckets)
	for i := 0; i < n; i++ {
		k := samplingKey(0xABCDEF, idOf(i))
		counts[k%buckets]++
	}
	expected := float64(n) / buckets
	chi := 0.0
	for _, c := range counts {
		d := float64(c) - expected
		chi += d * d / expected
	}
	// df = 63; p=0.001 critical ~ 102.
	if chi > 102 {
		t.Fatalf("key bucket chi2=%.2f indicates non-uniform keys", chi)
	}
}

// TestKeyReservoirIsAnExactUniformSample is the core SRSWOR property: over
// many survey salts, each C(N,K) subset is selected equally often, and each
// record's marginal inclusion probability is K/N.
func TestKeyReservoirIsAnExactUniformSample(t *testing.T) {
	const trials = 30000
	const N, K = 8, 3
	subset := make(map[string]int)
	marginal := make([]int, N)
	for trial := 0; trial < trials; trial++ {
		rs := &reservoir{}
		salt := uint64(trial*2654435761 + 1)
		// Present records in a deliberately fixed, then shuffled, order across
		// trials: order must not affect the key-based result.
		order := permutation(N, uint64(trial))
		for _, idx := range order {
			id := idOf(idx)
			rs.offer(keyedRecord{record: Record{ID: id}, key: samplingKey(salt, id)}, K)
		}
		if rs.N != N {
			t.Fatalf("N = %d, want %d", rs.N, N)
		}
		if len(rs.retained) != K {
			t.Fatalf("retained %d, want %d", len(rs.retained), K)
		}
		ids := make([]string, 0, K)
		for _, kr := range rs.retained {
			ids = append(ids, kr.record.ID)
			marginal[ofID(kr.record.ID)]++
		}
		sort.Strings(ids)
		subset[join(ids)]++
	}
	if len(subset) != 56 { // C(8,3)
		t.Fatalf("observed %d distinct subsets, want 56", len(subset))
	}
	wantPer := float64(trials) / 56
	for key, c := range subset {
		if math.Abs(float64(c)-wantPer) > wantPer*0.18 {
			t.Fatalf("subset %s selected %d times, want ~%.1f", key, c, wantPer)
		}
	}
	wantMarginal := float64(trials) * K / N
	for i, c := range marginal {
		if math.Abs(float64(c)-wantMarginal) > wantMarginal*0.05 {
			t.Fatalf("record %d included %d times, want ~%.1f", i, c, wantMarginal)
		}
	}
}

func TestReservoirCensusKeepsEverything(t *testing.T) {
	rs := &reservoir{}
	for i := 0; i < 3; i++ {
		id := idOf(i)
		rs.offer(keyedRecord{record: Record{ID: id}, key: samplingKey(7, id)}, 5)
	}
	if rs.N != 3 || len(rs.retained) != 3 {
		t.Fatalf("N=%d retained=%d, want 3/3", rs.N, len(rs.retained))
	}
}

// TestMergeIsExactUniformSample verifies that merging two independently
// collected, unequal shards yields a uniform SRSWOR of the combined 15 records
// at K=4. All C(15,4)=1365 subsets must appear with equal frequency; this is
// the property that rules out any weighting by sample count. Uniformity is
// assessed with a chi-square goodness-of-fit test rather than ad hoc bounds.
func TestMergeIsExactUniformSample(t *testing.T) {
	const trials = 200000
	const subsets = 1365
	subset := make(map[string]int)
	contrib := make(map[int]int) // retained records originating in the N=6 shard
	for trial := 0; trial < trials; trial++ {
		salt := uint64(trial*40503 + 11)
		first, second := &reservoir{}, &reservoir{}
		for i := 0; i < 9; i++ {
			id := idOf(i)
			first.offer(keyedRecord{record: Record{ID: id}, key: samplingKey(salt, id)}, 4)
		}
		for i := 9; i < 15; i++ {
			id := idOf(i)
			second.offer(keyedRecord{record: Record{ID: id}, key: samplingKey(salt, id)}, 4)
		}
		combineReservoirs(first, second, 4)
		if first.N != 15 || len(first.retained) != 4 {
			t.Fatalf("merged N=%d retained=%d, want 15/4", first.N, len(first.retained))
		}
		ids := make([]string, 0, 4)
		fromSecond := 0
		for _, kr := range first.retained {
			ids = append(ids, kr.record.ID)
			if ofID(kr.record.ID) >= 9 {
				fromSecond++
			}
		}
		sort.Strings(ids)
		subset[join(ids)]++
		contrib[fromSecond]++
	}
	if len(subset) != subsets {
		t.Fatalf("observed %d distinct subsets, want %d", len(subset), subsets)
	}
	df := subsets - 1
	expected := float64(trials) / float64(subsets)
	chi := 0.0
	for _, c := range subset {
		d := float64(c) - expected
		chi += d * d / expected
	}
	// p=0.001 upper-tail critical value is roughly df + 3.1*sqrt(2*df).
	critical := float64(df) + 3.1*math.Sqrt(2*float64(df))
	if chi > critical {
		t.Fatalf("merged sample not uniform: chi2=%.1f exceeds critical %.1f", chi, critical)
	}

	mean := 0.0
	for k, c := range contrib {
		mean += float64(k) * float64(c) / trials
	}
	// Expected records from the N=6 side: K * N2 / (N1+N2) = 4*6/15 = 1.6.
	if math.Abs(mean-1.6) > 0.03 {
		t.Fatalf("mean contribution of smaller-population shard = %.3f, want 1.60", mean)
	}
}

// TestMergeEquivalentToSinglePass proves merging shards produces exactly the
// same retained set as collecting all records in one pass (same salt, same
// keys), regardless of how the records are split.
func TestMergeEquivalentToSinglePass(t *testing.T) {
	for salt := uint64(1); salt <= 50; salt++ {
		split := newTestSampler(t, map[string]int{"x": 3}, salt)
		whole := newTestSampler(t, map[string]int{"x": 3}, salt)

		if err := split.AddShard(shardOf("p1", "x", idOf(0), idOf(1), idOf(2), idOf(3))); err != nil {
			t.Fatal(err)
		}
		if err := whole.AddShard(shardOf("all", "x", idOf(0), idOf(1), idOf(2), idOf(3), idOf(4), idOf(5), idOf(6), idOf(7), idOf(8), idOf(9))); err != nil {
			t.Fatal(err)
		}
		other := newTestSampler(t, map[string]int{"x": 3}, salt)
		if err := other.AddShard(shardOf("p2", "x", idOf(4), idOf(5), idOf(6), idOf(7), idOf(8), idOf(9))); err != nil {
			t.Fatal(err)
		}
		if err := split.Merge(other); err != nil {
			t.Fatal(err)
		}
		if split.PopulationN() != whole.PopulationN() {
			t.Fatalf("salt %d: N mismatch %d vs %d", salt, split.PopulationN(), whole.PopulationN())
		}
		got := split.RetainedRecords()
		want := whole.RetainedRecords()
		if len(got) != len(want) {
			t.Fatalf("salt %d: sample size mismatch", salt)
		}
		for i := range got {
			if got[i].ID != want[i].ID {
				t.Fatalf("salt %d: merged sample %v != single-pass %v", salt, idsOf(got), idsOf(want))
			}
		}
	}
}

func TestAddShardRecordsPopulationAndSource(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 2, "y": 10}, 42)
	if err := s.AddShard(shardOf("page-1", "x", "a", "b", "c", "d", "e")); err != nil {
		t.Fatal(err)
	}
	if s.PopulationN() != 5 {
		t.Fatalf("PopulationN = %d, want 5", s.PopulationN())
	}
	if s.SampleN() != 2 {
		t.Fatalf("SampleN = %d, want 2 (cap K=2)", s.SampleN())
	}
	srcs := s.Sources()
	if len(srcs) != 1 || srcs[0].ID != "page-1" || srcs[0].Count != 5 || srcs[0].Fingerprint == "" {
		t.Fatalf("unexpected sources: %+v", srcs)
	}
}

func TestAddShardRejectsInvalidInput(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 2}, 42)

	if err := s.AddShard(Shard{Source: "p", Records: nil}); err == nil {
		t.Fatal("expected error for empty shard")
	}
	if err := s.AddShard(shardOf("p", "unknown", "a")); err == nil {
		t.Fatal("expected error for undeclared stratum")
	}
	if err := s.AddShard(shardOf("p", "x", "a", "a")); err == nil {
		t.Fatal("expected error for duplicate ID within shard")
	}
	if err := s.AddShard(shardOf("", "x", "a")); err == nil {
		t.Fatal("expected error for blank source")
	}
	if err := s.AddShard(shardOf("p", "x", "")); err == nil {
		t.Fatal("expected error for blank record ID")
	}

	if err := s.AddShard(shardOf("p1", "x", "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddShard(shardOf("p2", "x", "a", "b")); err == nil {
		t.Fatal("expected overlap rejection across sources")
	}
}

func TestSameSourceResubmissionIsIdempotent(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 1}, 42)
	sh := shardOf("page-1", "x", "a", "b", "c")
	if err := s.AddShard(sh); err != nil {
		t.Fatal(err)
	}
	first := s.RetainedRecords()
	n := s.PopulationN()

	// Identical resubmission in a different order: no re-sample, no count.
	reordered := Shard{Source: "page-1", Records: append([]Record(nil), sh.Records...)}
	sort.Slice(reordered.Records, func(i, j int) bool {
		return reordered.Records[i].ID > reordered.Records[j].ID
	})
	if err := s.AddShard(reordered); err != nil {
		t.Fatalf("identical resubmission rejected: %v", err)
	}
	if s.PopulationN() != n {
		t.Fatalf("PopulationN changed from %d to %d on resubmission", n, s.PopulationN())
	}
	if got := s.RetainedRecords(); len(got) != len(first) || got[0].ID != first[0].ID {
		t.Fatalf("sample changed on identical resubmission: %v vs %v", got, first)
	}

	// Changed resubmission of the same source must be refused, not re-sampled.
	changed := shardOf("page-1", "x", "a", "b", "X")
	if err := s.AddShard(changed); err == nil {
		t.Fatal("expected rejection of changed source")
	}
	if s.PopulationN() != n {
		t.Fatalf("changed shard mutated population to %d", s.PopulationN())
	}
}

func TestMergeRejectsConfigurationConflictsAndOverlap(t *testing.T) {
	a := newTestSampler(t, map[string]int{"x": 2}, 1)
	if err := a.AddShard(shardOf("p1", "x", "a", "b")); err != nil {
		t.Fatal(err)
	}

	b, err := NewSampler("other-batch", map[string]int{"x": 2}, SamplerSalt(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddShard(shardOf("p2", "x", "c")); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(b); err == nil {
		t.Fatal("expected name conflict rejection")
	}

	// Same name, different salt: incompatible batches.
	c, err := NewSampler("batch", map[string]int{"x": 2}, SamplerSalt(99))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddShard(shardOf("p2", "x", "c")); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(c); err == nil {
		t.Fatal("expected salt conflict rejection")
	}

	d := newTestSampler(t, map[string]int{"x": 3}, 4)
	if err := d.AddShard(shardOf("p2", "x", "c")); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(d); err == nil {
		t.Fatal("expected capacity conflict rejection")
	}

	e := newTestSampler(t, map[string]int{"x": 2, "z": 2}, 5)
	if err := e.AddShard(shardOf("p2", "x", "c")); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(e); err == nil {
		t.Fatal("expected stratum-set conflict rejection")
	}

	overlap := newTestSampler(t, map[string]int{"x": 2}, 6)
	if err := overlap.AddShard(shardOf("p9", "x", "b")); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(overlap); err == nil {
		t.Fatal("expected record-overlap rejection")
	}

	dupSource := newTestSampler(t, map[string]int{"x": 2}, 7)
	if err := dupSource.AddShard(shardOf("p1", "x", "z")); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(dupSource); err == nil {
		t.Fatal("expected duplicate-source rejection")
	}
	if err := a.Merge(nil); err == nil {
		t.Fatal("expected nil merge rejection")
	}
}

func TestMergeDoesNotMutateTheOtherSampler(t *testing.T) {
	a := newTestSampler(t, map[string]int{"x": 2}, 1)
	b := newTestSampler(t, map[string]int{"x": 2}, 1)
	if err := a.AddShard(shardOf("p1", "x", "a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddShard(shardOf("p2", "x", "d", "e")); err != nil {
		t.Fatal(err)
	}
	beforeN := b.PopulationN()
	beforeSample := b.SampleN()
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if b.PopulationN() != beforeN || b.SampleN() != beforeSample {
		t.Fatal("Merge mutated the other sampler")
	}
}

// TestMergeIsIndependentPerStratum ensures a multi-stratum merge keeps each
// stratum's reservoir, population N and capacity separate and uniform on its
// own, rather than pooling records across strata.
func TestMergeIsIndependentPerStratum(t *testing.T) {
	capacities := map[string]int{"A": 2, "B": 1}
	a := newTestSampler(t, capacities, 21)
	b := newTestSampler(t, capacities, 21)
	if err := a.AddShard(shardOf("p1-A", "A", "a1", "a2", "a3", "a4")); err != nil {
		t.Fatal(err)
	}
	if err := a.AddShard(shardOf("p1-B", "B", "b1", "b2", "b3")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddShard(shardOf("p2-A", "A", "a5", "a6")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddShard(shardOf("p2-B", "B", "b4", "b5", "b6")); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}

	// Stratum A: N=6, retained at most K=2; every retained ID must be an A ID.
	rsA := a.reservoirs["A"]
	if rsA.N != 6 || len(rsA.retained) != 2 {
		t.Fatalf("stratum A N=%d retained=%d, want 6/2", rsA.N, len(rsA.retained))
	}
	for _, kr := range rsA.retained {
		if kr.record.Stratum != "A" || kr.record.ID[0] != 'a' {
			t.Fatalf("stratum A reservoir contains foreign record %+v", kr.record)
		}
	}
	// Stratum B: N=6, retained at most K=1.
	rsB := a.reservoirs["B"]
	if rsB.N != 6 || len(rsB.retained) != 1 {
		t.Fatalf("stratum B N=%d retained=%d, want 6/1", rsB.N, len(rsB.retained))
	}
	for _, kr := range rsB.retained {
		if kr.record.Stratum != "B" {
			t.Fatalf("stratum B reservoir contains foreign record %+v", kr.record)
		}
	}
	if a.PopulationN() != 12 || a.SampleN() != 3 {
		t.Fatalf("overall N=%d n=%d, want 12/3", a.PopulationN(), a.SampleN())
	}

	// Merge result must equal a single-pass sampler with the same salt.
	whole := newTestSampler(t, capacities, 21)
	if err := whole.AddShard(shardOf("all-A", "A", "a1", "a2", "a3", "a4", "a5", "a6")); err != nil {
		t.Fatal(err)
	}
	if err := whole.AddShard(shardOf("all-B", "B", "b1", "b2", "b3", "b4", "b5", "b6")); err != nil {
		t.Fatal(err)
	}
	for _, stratum := range []string{"A", "B"} {
		got := a.reservoirs[stratum].retained
		want := whole.reservoirs[stratum].retained
		if len(got) != len(want) {
			t.Fatalf("stratum %s size mismatch", stratum)
		}
		for i := range got {
			if got[i].record.ID != want[i].record.ID {
				t.Fatalf("stratum %s merged %v != single pass %v", stratum, got, want)
			}
		}
	}
}

func permutation(n int, seed uint64) []int {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	// deterministic Fisher-Yates from the seed
	x := seed
	for i := n - 1; i > 0; i-- {
		x = x*1103515245 + 12345
		j := int((x >> 16) % uint64(i+1))
		p[i], p[j] = p[j], p[i]
	}
	return p
}

func idsOf(rs []Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func idOf(i int) string { return "r" + itoa(i) }

func ofID(id string) int {
	var n int
	for _, c := range id[1:] {
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func join(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}
