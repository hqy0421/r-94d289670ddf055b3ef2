package agouti

import (
	"bytes"
	"math"
	"testing"
)

func amountShard(source, stratum string, amounts map[string]float64) Shard {
	records := make([]Record, 0, len(amounts))
	for id, amount := range amounts {
		records = append(records, Record{ID: id, Stratum: stratum, Amount: amount})
	}
	return Shard{Source: source, Records: records}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestReportEmptyErrors(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 2}, 1)
	if _, err := s.Report(); err == nil {
		t.Fatal("expected error reporting an empty sampler")
	}
}

// TestReportCensusHasZeroError: when every record is retained, the total and
// mean are exact and the finite-population standard error is exactly zero.
func TestReportCensusHasZeroError(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 10}, 1)
	if err := s.AddShard(amountShard("p1", "x", map[string]float64{
		"a": 10, "b": 20, "c": 30, "d": 40,
	})); err != nil {
		t.Fatal(err)
	}
	report, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	if report.N != 4 || report.n != 4 {
		t.Fatalf("N=%d n=%d, want 4/4", report.N, report.n)
	}
	if !approx(report.TotalAmount, 100) {
		t.Fatalf("total = %v, want 100", report.TotalAmount)
	}
	if !approx(report.MeanAmount, 25) {
		t.Fatalf("mean = %v, want 25", report.MeanAmount)
	}
	if report.StandardError != 0 {
		t.Fatalf("census standard error = %v, want 0", report.StandardError)
	}
	if !report.Estimable {
		t.Fatal("census must be estimable")
	}
	if len(report.Strata) != 1 || !report.Strata[0].Census {
		t.Fatal("stratum should be marked census")
	}
}

// TestReportStratifiedNOverNWeighting checks the textbook stratified
// estimator over two strata with different sampling fractions, including the
// finite-population-corrected variance:
//
//	hat(tau) = sum_h N_h * mean_h
//	Var(hat(tau)) = sum_h N_h^2 (1 - n_h/N_h) s_h^2 / n_h
func TestReportStratifiedNOverNWeighting(t *testing.T) {
	s := newTestSampler(t, map[string]int{"A": 2, "B": 2}, 7)
	// Stratum A: population N=4, census of two sampled values would be wrong to
	// assume; here n=2 of N=4. Amounts 10 and 20.
	if err := s.AddShard(amountShard("p1", "A", map[string]float64{
		"a1": 10, "a2": 20, "a3": 30, "a4": 40,
	})); err != nil {
		t.Fatal(err)
	}
	// Stratum B: population N=3, n=2. Amounts 5 and 15 sampled.
	if err := s.AddShard(amountShard("p2", "B", map[string]float64{
		"b1": 5, "b2": 15, "b3": 25,
	})); err != nil {
		t.Fatal(err)
	}
	report, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	if report.N != 7 || report.n != 4 {
		t.Fatalf("N=%d n=%d, want 7/4", report.N, report.n)
	}

	// Which records are retained depends on the keys; recompute expectations
	// from the actual retained sample so the test is key-scheme independent.
	var sumA, sumB float64
	var nA, nB, NA, NB int
	for _, sr := range report.Strata {
		rs := s.reservoirs[sr.Name]
		n := len(rs.retained)
		sum := 0.0
		for _, kr := range rs.retained {
			v, _ := kr.record.value(false)
			sum += v
		}
		switch sr.Name {
		case "A":
			sumA, nA, NA = sum, n, rs.N
		case "B":
			sumB, nB, NB = sum, n, rs.N
		}
	}
	wantTotal := float64(NA)*(sumA/float64(nA)) + float64(NB)*(sumB/float64(nB))
	if !approx(report.TotalAmount, wantTotal) {
		t.Fatalf("total = %v, want %v", report.TotalAmount, wantTotal)
	}
	if !approx(report.MeanAmount, wantTotal/float64(report.N)) {
		t.Fatalf("mean = %v, want %v", report.MeanAmount, wantTotal/float64(report.N))
	}

	// Independent FPC variance computation.
	var wantVar float64
	for _, sr := range report.Strata {
		if sr.Census || sr.n <= 1 {
			continue
		}
		rs := s.reservoirs[sr.Name]
		mean := 0.0
		for _, kr := range rs.retained {
			v, _ := kr.record.value(false)
			mean += v
		}
		mean /= float64(sr.n)
		sq := 0.0
		for _, kr := range rs.retained {
			v, _ := kr.record.value(false)
			sq += (v - mean) * (v - mean)
		}
		s2 := sq / float64(sr.n-1)
		Nh, nh := float64(sr.N), float64(sr.n)
		wantVar += Nh * Nh * (1 - nh/Nh) * s2 / nh
	}
	if !report.Estimable {
		t.Fatalf("should be estimable: %s", report.NotEstimableReason)
	}
	if !approx(report.StandardError, math.Sqrt(wantVar)) {
		t.Fatalf("SE = %v, want %v", report.StandardError, math.Sqrt(wantVar))
	}
}

// TestReportSingleSampleNonCensusNotEstimable: a non-census stratum with one
// retained record cannot yield a variance estimate. The point estimates stay
// valid; the SE is NaN and the report says why.
func TestReportSingleSampleNonCensusNotEstimable(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 1}, 3)
	if err := s.AddShard(amountShard("p1", "x", map[string]float64{
		"a": 10, "b": 20, "c": 30, "d": 40, "e": 50,
	})); err != nil {
		t.Fatal(err)
	}
	report, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	if report.n != 1 || report.N != 5 {
		t.Fatalf("n=%d N=%d, want 1/5", report.n, report.N)
	}
	if report.Estimable {
		t.Fatal("single-sample non-census must not be estimable")
	}
	if !math.IsNaN(report.StandardError) {
		t.Fatalf("SE = %v, want NaN", report.StandardError)
	}
	if report.NotEstimableReason == "" {
		t.Fatal("expected a reason for the unestimable variance")
	}
	// Point estimate remains N * the single observed amount.
	if !approx(report.TotalAmount, float64(report.N)*report.Strata[0].SampleMean) {
		t.Fatalf("point estimate %v not N/n weighted", report.TotalAmount)
	}
}

// TestReportMixedCensusAndSampledStrata: a census stratum contributes zero
// variance; a sampled stratum contributes its FPC term.
func TestReportMixedCensusAndSampledStrata(t *testing.T) {
	s := newTestSampler(t, map[string]int{"A": 10, "B": 1}, 11)
	if err := s.AddShard(amountShard("p1", "A", map[string]float64{"a1": 4, "a2": 8, "a3": 6, "a4": 10})); err != nil {
		t.Fatal(err)
	}
	if err := s.AddShard(amountShard("p2", "B", map[string]float64{"b1": 100, "b2": 200, "b3": 300})); err != nil {
		t.Fatal(err)
	}
	report, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	var censusFound, sampledFound bool
	for _, sr := range report.Strata {
		if sr.Census {
			censusFound = true
			// A census stratum contributes zero sampling-error variance because
			// its FPC factor (1 - n/N) is zero; its SE contribution is zero.
			v, ok := stratumTotalVariance(sr)
			if !ok || v != 0 {
				t.Fatalf("census stratum %s must contribute zero variance, got %v ok=%v", sr.Name, v, ok)
			}
			continue
		}
		sampledFound = true
		rs := s.reservoirs[sr.Name]
		if len(rs.retained) != 1 {
			t.Fatalf("expected single retained record in %s", sr.Name)
		}
	}
	if !censusFound || !sampledFound {
		t.Fatalf("expected one census and one sampled stratum, got %+v", report.Strata)
	}
	if report.Estimable || !math.IsNaN(report.StandardError) {
		t.Fatal("mixed report with a single-sample non-census stratum must be unestimable")
	}
}

func TestAmountTextParsedAtReportTime(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 10}, 5)
	sh := Shard{Source: "p1", Records: []Record{
		{ID: "a", Stratum: "x", AmountText: " 12.5 "},
		{ID: "b", Stratum: "x", AmountText: "7.5"},
	}}
	if err := s.AddShard(sh); err != nil {
		t.Fatal(err)
	}
	report, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	if !approx(report.MeanAmount, 10) {
		t.Fatalf("mean = %v, want 10", report.MeanAmount)
	}
	if !approx(report.TotalAmount, 20) {
		t.Fatalf("total = %v, want 20", report.TotalAmount)
	}
}

func TestStrictAmountRejectsUnparseable(t *testing.T) {
	s, err := NewSampler("batch", map[string]int{"x": 2}, SamplerSalt(1), SamplerStrictAmount())
	if err != nil {
		t.Fatal(err)
	}
	bad := Shard{Source: "p1", Records: []Record{{ID: "a", Stratum: "x", AmountText: "not-a-number"}}}
	if err := s.AddShard(bad); err == nil {
		t.Fatal("expected strict amount parse rejection")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 2, "y": 3}, 42)
	if err := s.AddShard(amountShard("p1", "x", map[string]float64{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5})); err != nil {
		t.Fatal(err)
	}
	if err := s.AddShard(amountShard("p2", "y", map[string]float64{"f": 6, "g": 7})); err != nil {
		t.Fatal(err)
	}
	before, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		t.Fatal(err)
	}
	// State must not embed unselected record IDs (only retained ones).
	retained := map[string]bool{}
	for _, r := range s.RetainedRecords() {
		retained[r.ID] = true
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !retained[id] && bytes.Contains(buf.Bytes(), []byte(`"id": "`+id+`"`)) {
			t.Fatalf("unselected record %q was persisted", id)
		}
	}

	reloaded, err := LoadSampler(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Name() != s.Name() || reloaded.PopulationN() != s.PopulationN() || reloaded.SampleN() != s.SampleN() {
		t.Fatal("reloaded sampler metadata mismatch")
	}
	after, err := reloaded.Report()
	if err != nil {
		t.Fatal(err)
	}
	if !approx(after.TotalAmount, before.TotalAmount) || !approx(after.MeanAmount, before.MeanAmount) ||
		!approx(after.StandardError, before.StandardError) {
		t.Fatalf("reports differ after reload: %+v vs %+v", before, after)
	}
}
