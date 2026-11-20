package agouti

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Record is one observed population unit. A Record is created only from data
// actually read off a page (see Sampler.CollectPage / CollectRows). A Record
// stored by a Sampler is always a record that was selected into the retained
// sample; the Sampler never stores records that were not selected.
type Record struct {
	// ID uniquely identifies the population unit across the whole survey.
	// Two different sources must never contain the same ID: such an overlap is
	// rejected. IDs may not be empty.
	ID string

	// Stratum is the declared stratum name the record belongs to. It must match
	// a stratum configured on the Sampler.
	Stratum string

	// Amount is the measured value of interest for the record. It may be left
	// zero and supplied instead via AmountText, which is parsed when a report
	// is generated.
	Amount float64

	// AmountText optionally holds the raw, on-page text representation of the
	// amount. It is parsed with strconv.ParseFloat only when Amount is zero and
	// AmountText is non-empty. The raw text is retained so that saved state does
	// not lose the value as read from the page.
	AmountText string
}

// keyedRecord pairs a retained record with its independent sampling key.
type keyedRecord struct {
	record Record
	key    uint64
}

// Shard is one complete, mutually-exclusive slice of the target population,
// read in full from a single logged-in page view. A shard is a *complete*
// fragment of the batch, not a convenience sample: every record the page
// exposes for that source must be included. Different shards must come from
// non-overlapping sources.
type Shard struct {
	// Source uniquely identifies where the shard came from (e.g. a page URL,
	// a page number, a batch partition key). Re-submitting a known Source is
	// idempotent only when the shard is identical to the first submission; a
	// changed shard for a known source is rejected.
	Source string

	// Records is the complete set of records read from the source. Record IDs
	// must be unique within a shard and disjoint from every other shard.
	Records []Record
}

// SourceInfo describes one accepted population source.
type SourceInfo struct {
	// ID is the source identifier (Shard.Source).
	ID string

	// Count is the actual number of records read from the source, as observed
	// on the page.
	Count int

	// Fingerprint is a SHA-256 digest of the canonical form of the shard. It is
	// used to detect a changed re-collection of the same source without storing
	// the unselected records themselves.
	Fingerprint string
}

// StratumReport is the per-stratum contribution to a sampling report.
type StratumReport struct {
	// Name is the configured stratum name.
	Name string

	// N is the actual stratum population size: the total number of records
	// observed across every accepted source for this stratum.
	N int

	// n is the number of records retained in the sample for this stratum.
	n int

	// K is the configured maximum sample size for this stratum.
	K int

	// SampleMean is the mean amount over the retained records.
	SampleMean float64

	// SampleVariance is the unbiased (1/(n-1)) sample variance of the retained
	// amounts. It is zero when n <= 1.
	SampleVariance float64

	// Census is true when the whole stratum was observed (n == N). A census
	// stratum has zero sampling error.
	Census bool

	// Estimable is false when this stratum's variance cannot be estimated: a
	// non-census stratum with exactly one retained record.
	Estimable bool

	// NotEstimableReason explains why Estimable is false. Empty otherwise.
	NotEstimableReason string
}

// Report is the stratified-sampling readout. All point estimates weight each
// stratum by its actual population size N, never by the number of sampled
// records.
type Report struct {
	// N is the actual overall population size across every stratum.
	N int

	// n is the total number of retained records.
	n int

	// TotalAmount is the N/n-weighted estimate of the overall population total.
	TotalAmount float64

	// MeanAmount is the estimated population mean amount.
	MeanAmount float64

	// StandardError is the finite-population-corrected standard error of the
	// estimated total. It is exactly zero when every stratum is a census. It is
	// set to NaN when the variance is not estimable; see Estimable.
	StandardError float64

	// Estimable reports whether StandardError could be estimated. It is false
	// when any non-census stratum retained exactly one record (variance cannot
	// be estimated from a single observation). Point estimates remain valid.
	Estimable bool

	// NotEstimableReason lists the strata that make the variance unestimable.
	NotEstimableReason string

	// Strata holds the per-stratum breakdown, ordered by configured stratum name.
	Strata []StratumReport

	// Sources lists every accepted source with its observed size and fingerprint.
	Sources []SourceInfo
}

// Sampler performs mergeable stratified sampling over complete shards read
// from logged-in pages.
//
// Within every declared stratum it keeps a simple random sample without
// replacement of at most K records and records the actual population size N.
// Shards of different sizes may be merged: every equal-sized subset of the
// combined population remains equally likely, because merging compares the
// fixed sampling keys of the retained records rather than weighting
// reservoirs by their sample counts.
//
// A Sampler never retains records that were not selected. Persisted state
// (Save) contains only the retained records, population sizes, source metadata
// and non-reversing membership bits, so it can be reopened to continue
// collection, merging and reporting.
type Sampler struct {
	name     string
	strata   []string
	capacity map[string]int

	// salt is the survey-wide randomization input for sampling keys. It is
	// generated once at construction and persisted, so reopened and merged
	// samplers derive identical keys for the same record and stay consistent.
	salt uint64

	// reservoirs maps stratum name to its reservoir.
	reservoirs map[string]*reservoir

	// sources maps source ID to accepted source metadata.
	sources map[string]SourceInfo

	// seenIDs holds every record ID observed so far, in memory only. It is used
	// to reject overlapping sources. It is never persisted.
	seenIDs map[string]bool

	// seenFilters stores, per stratum, a bit-only Bloom filter of every observed
	// record ID. Unlike seenIDs it is safe to persist without retaining the
	// unselected IDs, and it lets a reopened Sampler keep rejecting overlapping
	// sources with no false negatives.
	seenFilters map[string]*idFilter

	// strictAmount makes AmountText parse failures fatal when true.
	strictAmount bool
}

// reservoir holds one stratum's uniform without-replacement sample of at most
// K records together with the true seen population size.
//
// The sample is realized with the independent-key method: every observed
// record receives an independent uniform key derived as a cryptographic hash
// of (survey salt, record ID); the retained set is the K records with the
// smallest keys. Because keys are fixed properties of the records and are
// independent across records, the retained set is a simple random sample
// without replacement of min(N, K) from the observed population, and two
// reservoirs merge by simply keeping the K smallest keys across both — no new
// randomness and no weighting by sample count.
type reservoir struct {
	// N is the number of records observed for this stratum across all sources.
	N int

	// retained is the current uniform sample of size min(N, K), kept ordered by
	// ascending sampling key (ties broken by record ID) so that retained[len-1]
	// is always the largest key and hence the most evictable record.
	retained []keyedRecord
}

// lessKey reports whether a must rank before b: smaller key first, with the
// record ID as a deterministic tie-breaker for the astronomically unlikely
// case of two records sharing a 64-bit sampling key.
func lessKey(a, b keyedRecord) bool {
	if a.key != b.key {
		return a.key < b.key
	}
	return a.record.ID < b.record.ID
}

// offer inserts one already-keyed record into the reservoir of capacity k.
func (rs *reservoir) offer(kr keyedRecord, k int) {
	rs.N++
	if len(rs.retained) < k {
		rs.insertSorted(kr)
		return
	}
	// retained is ascending by key; the last element has the largest key.
	if lessKey(kr, rs.retained[len(rs.retained)-1]) {
		rs.retained[len(rs.retained)-1] = kr
		rs.bubbleDown(len(rs.retained) - 1)
	}
}

// insertSorted inserts kr while keeping retained in ascending key order.
func (rs *reservoir) insertSorted(kr keyedRecord) {
	rs.retained = append(rs.retained, kr)
	rs.bubbleDown(len(rs.retained) - 1)
}

// bubbleDown moves the element at i toward the front while it ranks before its
// predecessor, restoring ascending order.
func (rs *reservoir) bubbleDown(i int) {
	for i > 0 && lessKey(rs.retained[i], rs.retained[i-1]) {
		rs.retained[i], rs.retained[i-1] = rs.retained[i-1], rs.retained[i]
		i--
	}
}

// combineReservoirs merges src into dst. The merged reservoir holds the k
// records with the smallest sampling keys across both populations, which is a
// uniform without-replacement sample of the combined population regardless of
// how the two shard sizes or sample lengths differ. No randomness is drawn:
// the keys already determine the answer, so neither side is weighted by sample
// count.
func combineReservoirs(dst, src *reservoir, k int) {
	combined := make([]keyedRecord, 0, len(dst.retained)+len(src.retained))
	combined = append(combined, dst.retained...)
	combined = append(combined, src.retained...)
	// Both inputs are in ascending key order; re-sort and keep the k smallest,
	// which remain ascending so the last element stays the most evictable.
	sort.Slice(combined, func(i, j int) bool { return lessKey(combined[i], combined[j]) })
	if len(combined) > k {
		combined = combined[:k]
	}
	dst.N += src.N
	dst.retained = combined
}

// samplingKey derives an independent uniform 64-bit key for a record from the
// survey salt and the record ID. SHA-256 makes the keys effectively
// independent uniform draws; the salt keeps two surveys over the same record
// IDs independent of each other. A key collision is astronomically unlikely
// and is handled deterministically by falling back to ID ordering.
func samplingKey(salt uint64, id string) uint64 {
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], salt)
	copy(buf[8:], []byte(id))
	sum := sha256.Sum256(buf[:])
	return binary.LittleEndian.Uint64(sum[:8])
}

// value resolves the numeric amount of a record, parsing AmountText only when
// the numeric Amount is zero and text is present.
func (r Record) value(strict bool) (float64, error) {
	if r.Amount != 0 {
		return r.Amount, nil
	}
	if strings.TrimSpace(r.AmountText) == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(r.AmountText), 64)
	if err != nil {
		if strict {
			return 0, fmt.Errorf("record %q has unparseable amount %q: %s", r.ID, r.AmountText, err)
		}
		return 0, nil
	}
	return v, nil
}

// canonicalShard renders a shard deterministically for fingerprinting. Only
// values actually read from the page are included; ordering is normalized so
// that an identical shard presented in a different order still matches.
func canonicalShard(shard Shard) string {
	lines := make([]string, 0, len(shard.Records))
	for _, r := range shard.Records {
		lines = append(lines, strings.Join([]string{
			r.ID,
			r.Stratum,
			strconv.FormatFloat(r.Amount, 'g', -1, 64),
			r.AmountText,
		}, "\x00"))
	}
	sort.Strings(lines)
	return shard.Source + "\n" + strings.Join(lines, "\n")
}

func fingerprint(shard Shard) string {
	sum := sha256.Sum256([]byte(canonicalShard(shard)))
	return hex.EncodeToString(sum[:])
}

// finitePopulationVariance accumulates the finite-population-corrected
// contribution of one stratum to the variance of the estimated total:
// N_h^2 * (1 - n_h/N_h) * s_h^2 / n_h. The caller sums these and takes the
// square root. Returns ok=false when the stratum is non-census and n == 1.
func stratumTotalVariance(r StratumReport) (float64, bool) {
	n := float64(r.n)
	N := float64(r.N)
	if r.Census {
		return 0, true
	}
	if r.n <= 1 {
		return 0, false
	}
	return N * N * (1 - n/N) * r.SampleVariance / n, true
}

// roundFp returns 0 for values within 1e-12 of zero, suppressing signed zeros
// and floating point dust in report numbers.
func roundFp(v float64) float64 {
	if math.Abs(v) < 1e-12 {
		return 0
	}
	return v
}
