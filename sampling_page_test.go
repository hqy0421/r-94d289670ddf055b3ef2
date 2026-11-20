package agouti

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sclevine/agouti/internal/mocks"
)

// fakeScriptPage returns canned JSON rows through the same RunScript contract
// a real *Page uses, so CollectPage is exercised against genuine page-reading
// plumbing without a browser.
type fakeScriptPage struct {
	rows []scriptRow
	err  error
}

func (f *fakeScriptPage) RunScript(body string, arguments map[string]interface{}, result interface{}) error {
	if f.err != nil {
		return f.err
	}
	// The production code passes selectors as arguments rather than building
	// them into the script; assert they travel through.
	for _, key := range []string{"rowSelector", "idSelector"} {
		if _, ok := arguments[key]; !ok {
			return errMissing(key)
		}
	}
	raw, _ := json.Marshal(f.rows)
	return json.Unmarshal(raw, result)
}

type errString string

func (e errString) Error() string { return string(e) }
func errMissing(k string) error   { return errString("missing argument " + k) }

func TestCollectPageReadsCompleteShard(t *testing.T) {
	s := newTestSampler(t, map[string]int{"tier-1": 2, "tier-2": 2}, 9)
	page := &fakeScriptPage{rows: []scriptRow{
		{ID: "row-1", Amount: "10.00", Stratum: "tier-1"},
		{ID: "row-2", Amount: "20.00", Stratum: "tier-1"},
		{ID: "row-3", Amount: "30.00", Stratum: "tier-2"},
	}}
	spec := PageShardSpec{
		Source:          "https://app.example.com/batch?page=1",
		RowSelector:     "table.transactions tr.record",
		IDSelector:      "td.id",
		AmountSelector:  "td.amount",
		StratumSelector: "td.tier",
	}
	if err := s.CollectPage(page, spec); err != nil {
		t.Fatal(err)
	}
	if s.PopulationN() != 3 {
		t.Fatalf("PopulationN = %d, want 3", s.PopulationN())
	}
	src := s.Sources()
	if len(src) != 1 || src[0].Count != 3 {
		t.Fatalf("sources = %+v", src)
	}
	report, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	if report.N != 3 {
		t.Fatalf("report N = %d, want 3", report.N)
	}
}

func TestCollectPageConstantStratum(t *testing.T) {
	s := newTestSampler(t, map[string]int{"only": 5}, 9)
	page := &fakeScriptPage{rows: []scriptRow{
		{ID: "x1", Amount: "1"},
		{ID: "x2", Amount: "2"},
	}}
	spec := PageShardSpec{
		Source:         "p",
		RowSelector:    "li",
		IDSelector:     ".id",
		AmountSelector: ".amt",
		Stratum:        "only",
	}
	if err := s.CollectPage(page, spec); err != nil {
		t.Fatal(err)
	}
	for _, sr := range reportStrata(t, s) {
		if sr.Name != "only" || sr.N != 2 {
			t.Fatalf("unexpected stratum state: %+v", sr)
		}
	}
}

func TestCollectPageRejectsBadSpecAndPageErrors(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 2}, 9)
	page := &fakeScriptPage{rows: []scriptRow{{ID: "a"}}}

	if err := s.CollectPage(nil, PageShardSpec{Source: "p", RowSelector: "r", IDSelector: "i", Stratum: "x"}); err == nil {
		t.Fatal("expected nil page rejection")
	}
	if err := s.CollectPage(page, PageShardSpec{RowSelector: "r", IDSelector: "i", Stratum: "x"}); err == nil {
		t.Fatal("expected missing source rejection")
	}
	if err := s.CollectPage(page, PageShardSpec{Source: "p", IDSelector: "i", Stratum: "x"}); err == nil {
		t.Fatal("expected missing row selector rejection")
	}
	if err := s.CollectPage(page, PageShardSpec{Source: "p", RowSelector: "r", Stratum: "x"}); err == nil {
		t.Fatal("expected missing id selector rejection")
	}
	if err := s.CollectPage(page, PageShardSpec{Source: "p", RowSelector: "r", IDSelector: "i"}); err == nil {
		t.Fatal("expected missing stratum rejection")
	}
	if err := s.CollectPage(page, PageShardSpec{Source: "p", RowSelector: "r", IDSelector: "i", Stratum: "x", StratumSelector: "td.t"}); err == nil {
		t.Fatal("expected conflicting stratum selector rejection")
	}

	failing := &fakeScriptPage{err: errString("session closed")}
	if err := s.CollectPage(failing, PageShardSpec{Source: "p", RowSelector: "r", IDSelector: "i", Stratum: "x"}); err == nil {
		t.Fatal("expected page read error to propagate")
	}
	if s.PopulationN() != 0 {
		t.Fatal("failed page read must not mutate the sampler")
	}
}

func TestCollectPageIdempotentAcrossRepeatedLogin(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 1}, 9)
	spec := PageShardSpec{Source: "p", RowSelector: "tr", IDSelector: ".id", Stratum: "x"}
	page1 := &fakeScriptPage{rows: []scriptRow{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	if err := s.CollectPage(page1, spec); err != nil {
		t.Fatal(err)
	}
	n := s.PopulationN()
	first := s.RetainedRecords()
	// Re-login and re-read the identical page: must be a no-op.
	page2 := &fakeScriptPage{rows: []scriptRow{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	if err := s.CollectPage(page2, spec); err != nil {
		t.Fatal(err)
	}
	if s.PopulationN() != n || len(s.RetainedRecords()) != len(first) {
		t.Fatal("identical page re-collection was not idempotent")
	}
	// A changed page under the same source URL must be refused.
	page3 := &fakeScriptPage{rows: []scriptRow{{ID: "a"}, {ID: "b"}, {ID: "zzz"}}}
	if err := s.CollectPage(page3, spec); err == nil {
		t.Fatal("expected changed source rejection")
	}
}

// TestCollectPageAgainstMockSession drives the real *Page.RunScript path via
// the project's session mock, proving the production page contract works.
func TestCollectPageAgainstMockSession(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 3}, 9)
	session := &mocks.Session{}
	session.ExecuteCall.Result = `[{"id":"m1","amount":"5","stratum":"x"},{"id":"m2","amount":"7","stratum":"x"}]`
	page := NewTestPage(session)

	spec := PageShardSpec{Source: "mock", RowSelector: "tr", IDSelector: ".id", AmountSelector: ".amt", Stratum: "x"}
	if err := s.CollectPage(page, spec); err != nil {
		t.Fatal(err)
	}
	if s.PopulationN() != 2 {
		t.Fatalf("PopulationN = %d, want 2", s.PopulationN())
	}
	if !strings.Contains(session.ExecuteCall.Body, "querySelectorAll") {
		t.Fatal("CollectPage did not issue a DOM-reading script")
	}
	report, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	if report.MeanAmount != 6 {
		t.Fatalf("mean = %v, want 6", report.MeanAmount)
	}
}

func TestCollectRowsValidates(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 2}, 9)
	if err := s.CollectRows("", []PageRow{{ID: "a", Stratum: "x"}}); err == nil {
		t.Fatal("expected blank source rejection")
	}
	if err := s.CollectRows("p", nil); err == nil {
		t.Fatal("expected empty rows rejection")
	}
	if err := s.CollectRows("p", []PageRow{{ID: "a"}}); err == nil {
		t.Fatal("expected missing stratum rejection")
	}
}

func TestReopenContinuesCollectionAndMerging(t *testing.T) {
	s := newTestSampler(t, map[string]int{"x": 2}, 77)
	if err := s.AddShard(shardOf("p1", "x", "a", "b", "c", "d")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		t.Fatal(err)
	}

	reopened, err := LoadSampler(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// Continue collecting from a new disjoint source.
	if err := reopened.AddShard(shardOf("p2", "x", "e", "f", "g")); err != nil {
		t.Fatal(err)
	}
	if reopened.PopulationN() != 7 {
		t.Fatalf("N after continue = %d, want 7", reopened.PopulationN())
	}
	// Re-adding the original source by ID is still idempotent after reopen.
	if err := reopened.AddShard(shardOf("p1", "x", "a", "b", "c", "d")); err != nil {
		t.Fatalf("known source should remain idempotent after reopen: %v", err)
	}
	if reopened.PopulationN() != 7 {
		t.Fatal("idempotent re-add after reopen changed N")
	}
	// Overlap with an originally-seen but unselected ID is still rejected via
	// the persisted membership filter.
	if err := reopened.AddShard(shardOf("p3", "x", "b", "z")); err == nil {
		t.Fatal("expected overlap rejection after reopen")
	}
	if _, err := reopened.Report(); err != nil {
		t.Fatal(err)
	}

	// A second independently-reopened copy merges cleanly when disjoint.
	other, err := LoadSampler(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.AddShard(shardOf("p4", "x", "h", "i")); err != nil {
		t.Fatal(err)
	}
	// Merge requires disjoint sources; reopen-copy still holds p1, so use a
	// fresh-built counterpart instead to test the happy path.
	fresh := newTestSampler(t, map[string]int{"x": 2}, 77)
	if err := fresh.AddShard(shardOf("p5", "x", "j", "k")); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Merge(fresh); err != nil {
		t.Fatalf("merge after reopen failed: %v", err)
	}
	if reopened.PopulationN() != 9 {
		t.Fatalf("N after merge = %d, want 9", reopened.PopulationN())
	}
}

func TestReopenPersistsOverlapDetection(t *testing.T) {
	// K=1 over 6 records: most observed IDs are unselected, yet the persisted
	// bit filter must still make every one of them detectable after reload.
	s := newTestSampler(t, map[string]int{"x": 1}, 77)
	if err := s.AddShard(shardOf("p1", "x", "a", "b", "c", "d", "e", "f")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadSampler(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := reloaded.AddShard(shardOf("other-"+id, "x", id, "fresh-"+id)); err == nil {
			t.Fatalf("overlap on previously observed (possibly unselected) ID %q not detected after reload", id)
		}
	}
	// A genuinely new disjoint source still works.
	if err := reloaded.AddShard(shardOf("p2", "x", "g", "h")); err != nil {
		t.Fatalf("disjoint source rejected after reload: %v", err)
	}
}

func TestMergeOfReloadedSamplersRejectsOverlap(t *testing.T) {
	build := func(strata map[string]int, source string, ids []string) *Sampler {
		s, err := NewSampler("batch", strata, SamplerSalt(88))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddShard(shardOf(source, "x", ids...)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	roundTrip := func(s *Sampler) *Sampler {
		var buf bytes.Buffer
		if err := s.Save(&buf); err != nil {
			t.Fatal(err)
		}
		out, err := LoadSampler(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Two disjoint surveys, each reloaded, merge fine.
	a := roundTrip(build(map[string]int{"x": 2}, "p1", []string{"a", "b", "c"}))
	b := roundTrip(build(map[string]int{"x": 2}, "p2", []string{"d", "e"}))
	if err := a.Merge(b); err != nil {
		t.Fatalf("disjoint reload merge failed: %v", err)
	}
	if a.PopulationN() != 5 {
		t.Fatalf("merged N = %d, want 5", a.PopulationN())
	}

	// Overlapping surveys, each reloaded. K=3 guarantees the shared ID is
	// retained on both sides, so the point-membership guard detects it even
	// though neither sampler keeps the other side's unselected records.
	c := roundTrip(build(map[string]int{"x": 3}, "p3", []string{"f", "g", "h"}))
	d := roundTrip(build(map[string]int{"x": 3}, "p4", []string{"g", "i", "j"}))
	if err := c.Merge(d); err == nil {
		t.Fatal("expected overlap rejection across reloaded samplers")
	}
}

func TestLoadRejectsCorruptState(t *testing.T) {
	if _, err := LoadSampler(strings.NewReader("{not json")); err == nil {
		t.Fatal("expected decode error")
	}
	good := func() []byte {
		s := newTestSampler(t, map[string]int{"x": 2}, 5)
		_ = s.AddShard(shardOf("p1", "x", "a", "b"))
		var b bytes.Buffer
		_ = s.Save(&b)
		return b.Bytes()
	}()
	doc := string(good)
	if _, err := LoadSampler(strings.NewReader(strings.Replace(doc, `"version": 1`, `"version": 99`, 1))); err == nil {
		t.Fatal("expected version mismatch rejection")
	}
	if _, err := LoadSampler(strings.NewReader(strings.Replace(doc, `"salt": 5`, `"salt": 6`, 1))); err == nil {
		t.Fatal("expected tampered-salt rejection (keys would mismatch)")
	}
}

func reportStrata(t *testing.T, s *Sampler) []StratumReport {
	r, err := s.Report()
	if err != nil {
		t.Fatal(err)
	}
	return r.Strata
}
