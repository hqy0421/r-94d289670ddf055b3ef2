package agouti

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"sort"
	"strings"
)

// SamplerOption configures a Sampler.
type SamplerOption func(*samplerConfig)

type samplerConfig struct {
	salt         uint64
	saltSet      bool
	strictAmount bool
}

// SamplerSalt sets the survey salt explicitly, making sampling fully
// deterministic: the same (salt, record ID) always yields the same sampling
// key. When unset a cryptographically random salt is generated. The salt is
// part of the persisted state so reopened and merged samplers stay consistent.
func SamplerSalt(salt uint64) SamplerOption {
	return func(c *samplerConfig) {
		c.salt = salt
		c.saltSet = true
	}
}

// SamplerStrictAmount makes AddShard and CollectPage reject records whose
// AmountText cannot be parsed as a number. By default an unparseable amount is
// treated as zero.
func SamplerStrictAmount() SamplerOption {
	return func(c *samplerConfig) { c.strictAmount = true }
}

// NewSampler creates a mergeable stratified sampler.
//
// name identifies the survey batch and must be non-empty. strata maps every
// declared stratum name to its maximum retained sample size K; K must be
// positive and the map non-empty. The declared configuration is immutable for
// the life of the survey: reopening saved state or merging another sampler
// with a different name, stratum set, per-stratum K, or survey salt is
// rejected.
func NewSampler(name string, strata map[string]int, options ...SamplerOption) (*Sampler, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("sampler name must not be empty")
	}
	if len(strata) == 0 {
		return nil, fmt.Errorf("at least one stratum must be declared")
	}

	names := make([]string, 0, len(strata))
	capacity := make(map[string]int, len(strata))
	for stratum, k := range strata {
		if strings.TrimSpace(stratum) == "" {
			return nil, fmt.Errorf("stratum names must not be empty")
		}
		if k <= 0 {
			return nil, fmt.Errorf("stratum %q capacity K must be positive, got %d", stratum, k)
		}
		names = append(names, stratum)
		capacity[stratum] = k
	}
	sort.Strings(names)

	cfg := samplerConfig{}
	for _, option := range options {
		option(&cfg)
	}
	salt := cfg.salt
	if !cfg.saltSet {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("failed to generate survey salt: %s", err)
		}
		salt = binary.LittleEndian.Uint64(b[:])
	}

	s := &Sampler{
		name:         name,
		strata:       names,
		capacity:     capacity,
		salt:         salt,
		reservoirs:   make(map[string]*reservoir),
		sources:      make(map[string]SourceInfo),
		seenIDs:      make(map[string]bool),
		seenFilters:  make(map[string]*idFilter),
		strictAmount: cfg.strictAmount,
	}
	for _, stratum := range names {
		s.reservoirs[stratum] = &reservoir{}
		s.seenFilters[stratum] = newIDFilter()
	}
	return s, nil
}

// Name returns the survey batch name.
func (s *Sampler) Name() string { return s.name }

// Strata returns the configured stratum names in sorted order.
func (s *Sampler) Strata() []string {
	out := make([]string, len(s.strata))
	copy(out, s.strata)
	return out
}

// PopulationN returns the actual overall population size observed so far.
func (s *Sampler) PopulationN() int {
	n := 0
	for _, r := range s.reservoirs {
		n += r.N
	}
	return n
}

// SampleN returns the total number of retained records.
func (s *Sampler) SampleN() int {
	n := 0
	for _, r := range s.reservoirs {
		n += len(r.retained)
	}
	return n
}

// Sources returns accepted source metadata ordered by source ID.
func (s *Sampler) Sources() []SourceInfo {
	out := make([]SourceInfo, 0, len(s.sources))
	for _, info := range s.sources {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RetainedRecords returns a copy of the retained sample, ordered by stratum
// (configured order) and then by record ID. Only selected records are ever
// available; unselected records are not stored and cannot be returned.
func (s *Sampler) RetainedRecords() []Record {
	var out []Record
	for _, stratum := range s.strata {
		rs := s.reservoirs[stratum]
		group := make([]keyedRecord, len(rs.retained))
		copy(group, rs.retained)
		sort.Slice(group, func(i, j int) bool { return lessKey(group[i], group[j]) })
		for _, kr := range group {
			out = append(out, kr.record)
		}
	}
	return out
}

// validateShard checks a shard against the declared configuration and against
// every source already accepted. It mutates no state.
func (s *Sampler) validateShard(shard Shard) error {
	if strings.TrimSpace(shard.Source) == "" {
		return fmt.Errorf("shard source must not be empty")
	}
	if len(shard.Records) == 0 {
		return fmt.Errorf("shard from source %q contains no records", shard.Source)
	}

	localIDs := make(map[string]bool, len(shard.Records))
	for _, r := range shard.Records {
		if r.ID == "" {
			return fmt.Errorf("source %q contains a record with an empty ID", shard.Source)
		}
		if _, ok := s.capacity[r.Stratum]; !ok {
			return fmt.Errorf("source %q record %q uses undeclared stratum %q", shard.Source, r.ID, r.Stratum)
		}
		if localIDs[r.ID] {
			return fmt.Errorf("source %q contains duplicate record ID %q", shard.Source, r.ID)
		}
		// Record IDs are globally unique across the survey, so check the
		// in-memory index and every stratum's persisted membership filter.
		if s.seenIDs[r.ID] {
			return fmt.Errorf("record %q from source %q overlaps an already accepted source", r.ID, shard.Source)
		}
		for stratum, filter := range s.seenFilters {
			if filter.contains(r.ID) {
				return fmt.Errorf("record %q from source %q overlaps an already accepted source (stratum %q)", r.ID, shard.Source, stratum)
			}
		}
		if _, err := r.value(s.strictAmount); err != nil {
			return err
		}
		localIDs[r.ID] = true
	}
	return nil
}

// AddShard incorporates one complete shard read from a single source.
//
// Re-submitting an already accepted source is idempotent: an identical shard
// changes nothing and is not re-sampled or counted again. A changed shard for
// a known source is rejected, as is any overlap with another source.
func (s *Sampler) AddShard(shard Shard) error {
	if prior, known := s.sources[shard.Source]; known {
		fp := fingerprint(shard)
		if fp != prior.Fingerprint {
			return fmt.Errorf("source %q changed since first collection: refusing to re-sample", shard.Source)
		}
		return nil // identical re-collection: no re-sampling, no accumulation
	}

	if err := s.validateShard(shard); err != nil {
		return err
	}

	// All checks passed; commit. Each record gets its own independent key.
	for _, r := range shard.Records {
		k := s.capacity[r.Stratum]
		s.reservoirs[r.Stratum].offer(keyedRecord{record: r, key: samplingKey(s.salt, r.ID)}, k)
		s.seenIDs[r.ID] = true
		s.seenFilters[r.Stratum].add(r.ID)
	}
	s.sources[shard.Source] = SourceInfo{
		ID:          shard.Source,
		Count:       len(shard.Records),
		Fingerprint: fingerprint(shard),
	}
	return nil
}

// Merge combines another sampler's survey state into the receiver.
//
// The two samplers must describe the same batch: identical name, salt, stratum
// set and per-stratum capacities. Their accepted sources and observed records
// must be disjoint. After merging, each stratum holds a uniform
// without-replacement sample of the combined population, obtained by keeping
// the smallest sampling keys across both sides — the contribution of each side
// follows its population size, never the number of records it retained. The
// other sampler is left unchanged.
func (s *Sampler) Merge(other *Sampler) error {
	if other == nil {
		return fmt.Errorf("cannot merge a nil sampler")
	}
	if other.name != s.name {
		return fmt.Errorf("configuration conflict: sampler names %q and %q differ", s.name, other.name)
	}
	if other.salt != s.salt {
		return fmt.Errorf("configuration conflict: survey salts differ; samplers are not from the same batch")
	}
	if len(other.strata) != len(s.strata) {
		return fmt.Errorf("configuration conflict: stratum sets differ")
	}
	for _, stratum := range s.strata {
		otherK, ok := other.capacity[stratum]
		if !ok {
			return fmt.Errorf("configuration conflict: stratum %q missing from merged sampler", stratum)
		}
		if otherK != s.capacity[stratum] {
			return fmt.Errorf("configuration conflict: stratum %q capacity %d != %d", stratum, s.capacity[stratum], otherK)
		}
	}

	for id := range other.sources {
		if _, collision := s.sources[id]; collision {
			return fmt.Errorf("merge refused: source %q is present in both samplers (overlapping sources)", id)
		}
	}
	// Overlap must be rejected even when the colliding ID was not retained.
	// Record IDs are globally unique, so every observed ID on each side is
	// tested against the other side's full set of persisted membership
	// filters. (After a save/reload, evicted IDs live only in those filters.)
	for id := range other.seenIDs {
		if s.seenIDs[id] {
			return fmt.Errorf("merge refused: record %q overlaps between samplers", id)
		}
	}
	for _, filter := range s.seenFilters {
		for id := range other.seenIDs {
			if filter.contains(id) {
				return fmt.Errorf("merge refused: record %q overlaps between samplers", id)
			}
		}
	}
	for _, otherFilter := range other.seenFilters {
		for id := range s.seenIDs {
			if otherFilter.contains(id) {
				return fmt.Errorf("merge refused: record %q overlaps between samplers", id)
			}
		}
	}

	// All error conditions above were checked before any mutation.
	for _, stratum := range s.strata {
		combineReservoirs(s.reservoirs[stratum], other.reservoirs[stratum], s.capacity[stratum])
	}
	for id, info := range other.sources {
		s.sources[id] = info
	}
	for id := range other.seenIDs {
		s.seenIDs[id] = true
	}
	for _, stratum := range s.strata {
		s.seenFilters[stratum].union(other.seenFilters[stratum])
	}
	return nil
}

// Report computes the N/n-weighted population estimates from the retained
// records. It returns an error only when no population has been observed.
func (s *Sampler) Report() (*Report, error) {
	totalN := s.PopulationN()
	if totalN == 0 {
		return nil, fmt.Errorf("no records collected; nothing to report")
	}

	report := &Report{N: totalN, Estimable: true}
	var totalEstimate, totalVariance float64
	var blocked []string

	for _, stratum := range s.strata {
		rs := s.reservoirs[stratum]
		if rs.N == 0 {
			continue
		}

		n := len(rs.retained)
		sum := 0.0
		values := make([]float64, n)
		for i, kr := range rs.retained {
			v, err := kr.record.value(s.strictAmount)
			if err != nil {
				return nil, err
			}
			values[i] = v
			sum += v
		}
		mean := sum / float64(n)

		variance := 0.0
		if n > 1 {
			sq := 0.0
			for _, v := range values {
				d := v - mean
				sq += d * d
			}
			variance = sq / float64(n-1)
		}

		census := n == rs.N
		estimable := census || n > 1
		sr := StratumReport{
			Name:           stratum,
			N:              rs.N,
			n:              n,
			K:              s.capacity[stratum],
			SampleMean:     roundFp(mean),
			SampleVariance: roundFp(variance),
			Census:         census,
			Estimable:      estimable,
		}
		if !estimable {
			sr.NotEstimableReason = fmt.Sprintf("stratum %q: non-census population N=%d with single sample n=1; variance not estimable", stratum, rs.N)
			blocked = append(blocked, stratum)
			report.Estimable = false
		}

		// Stratum total estimator: N_h * mean_h.
		totalEstimate += float64(rs.N) * mean

		if v, ok := stratumTotalVariance(sr); ok {
			totalVariance += v
		}
		report.Strata = append(report.Strata, sr)
		report.n += n
	}

	report.TotalAmount = roundFp(totalEstimate)
	report.MeanAmount = roundFp(totalEstimate / float64(totalN))

	if report.Estimable {
		report.StandardError = roundFp(math.Sqrt(totalVariance))
	} else {
		report.StandardError = math.NaN()
		report.NotEstimableReason = "finite-population standard error not estimable for non-census single-sample stratum/strata: " + strings.Join(blocked, ", ")
	}
	report.Sources = s.Sources()
	return report, nil
}

// ----- Persistence -----
//
// The persisted document contains only:
//   - the immutable survey configuration (name, salt, strata, capacities),
//   - per stratum: the actual N, the retained records (with their sampling
//     keys) and a bit-only membership filter of observed record IDs,
//   - accepted source metadata (ID, observed count, fingerprint).
// It never contains the full set of unselected records.

type persistedRecord struct {
	ID         string  `json:"id"`
	Stratum    string  `json:"stratum"`
	Amount     float64 `json:"amount"`
	AmountText string  `json:"amount_text,omitempty"`
	Key        uint64  `json:"sampling_key"`
}

type persistedReservoir struct {
	N        int               `json:"n_population"`
	Retained []persistedRecord `json:"retained"`
	IDFilter string            `json:"id_filter_hex"`
}

type persistedSource struct {
	ID          string `json:"id"`
	Count       int    `json:"count"`
	Fingerprint string `json:"fingerprint"`
}

type persistedSampler struct {
	Version      int                           `json:"version"`
	Name         string                        `json:"name"`
	Salt         uint64                        `json:"salt"`
	Strata       []string                      `json:"strata"`
	Capacities   map[string]int                `json:"capacities"`
	Reservoirs   map[string]persistedReservoir `json:"reservoirs"`
	Sources      []persistedSource             `json:"sources"`
	StrictAmount bool                          `json:"strict_amount"`
}

const samplerStateVersion = 1

// Save writes the survey state to w. The state is sufficient to continue
// collection, merging and reporting after reopening with LoadSampler.
func (s *Sampler) Save(w io.Writer) error {
	if w == nil {
		return fmt.Errorf("nil writer")
	}
	doc := persistedSampler{
		Version:      samplerStateVersion,
		Name:         s.name,
		Salt:         s.salt,
		Strata:       s.Strata(),
		Capacities:   make(map[string]int, len(s.capacity)),
		Reservoirs:   make(map[string]persistedReservoir, len(s.reservoirs)),
		StrictAmount: s.strictAmount,
	}
	for stratum, k := range s.capacity {
		doc.Capacities[stratum] = k
	}
	for stratum, rs := range s.reservoirs {
		pr := persistedReservoir{
			N:        rs.N,
			Retained: make([]persistedRecord, 0, len(rs.retained)),
			IDFilter: fmt.Sprintf("%x", s.seenFilters[stratum].bits),
		}
		for _, kr := range rs.retained {
			pr.Retained = append(pr.Retained, persistedRecord{
				ID:         kr.record.ID,
				Stratum:    kr.record.Stratum,
				Amount:     kr.record.Amount,
				AmountText: kr.record.AmountText,
				Key:        kr.key,
			})
		}
		doc.Reservoirs[stratum] = pr
	}
	for _, info := range s.Sources() {
		doc.Sources = append(doc.Sources, persistedSource{
			ID: info.ID, Count: info.Count, Fingerprint: info.Fingerprint,
		})
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		return fmt.Errorf("failed to encode sampler state: %s", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write sampler state: %s", err)
	}
	return nil
}

// SaveFile writes the survey state to a file.
func (s *Sampler) SaveFile(path string) error {
	if path == "" {
		return fmt.Errorf("empty state file path")
	}
	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		return err
	}
	if err := ioutil.WriteFile(path, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("failed to save sampler state: %s", err)
	}
	return nil
}

// LoadSampler restores a sampler previously written with Save. The stored
// configuration (name, salt, strata, capacities) is authoritative; a document
// with an unsupported version or internally inconsistent reservoirs is
// rejected. Reopened samplers derive the same sampling keys as the originals.
func LoadSampler(r io.Reader) (*Sampler, error) {
	if r == nil {
		return nil, fmt.Errorf("nil reader")
	}
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read sampler state: %s", err)
	}
	var doc persistedSampler
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to decode sampler state: %s", err)
	}
	if doc.Version != samplerStateVersion {
		return nil, fmt.Errorf("unsupported sampler state version %d (want %d)", doc.Version, samplerStateVersion)
	}
	if len(doc.Strata) == 0 || len(doc.Capacities) == 0 {
		return nil, fmt.Errorf("sampler state is missing its configuration")
	}

	s := &Sampler{
		name:         doc.Name,
		salt:         doc.Salt,
		strata:       append([]string(nil), doc.Strata...),
		capacity:     make(map[string]int, len(doc.Capacities)),
		reservoirs:   make(map[string]*reservoir, len(doc.Reservoirs)),
		sources:      make(map[string]SourceInfo, len(doc.Sources)),
		seenIDs:      make(map[string]bool),
		seenFilters:  make(map[string]*idFilter, len(doc.Reservoirs)),
		strictAmount: doc.StrictAmount,
	}
	for stratum, k := range doc.Capacities {
		if k <= 0 {
			return nil, fmt.Errorf("stored capacity for stratum %q is invalid: %d", stratum, k)
		}
		s.capacity[stratum] = k
	}

	seen := make(map[string]bool)
	for _, stratum := range s.strata {
		pr, ok := doc.Reservoirs[stratum]
		if !ok {
			return nil, fmt.Errorf("sampler state is missing reservoir for stratum %q", stratum)
		}
		if len(pr.Retained) > s.capacity[stratum] || len(pr.Retained) > pr.N {
			return nil, fmt.Errorf("sampler state for stratum %q is inconsistent", stratum)
		}
		rs := &reservoir{N: pr.N}
		for i, p := range pr.Retained {
			if p.Stratum != stratum {
				return nil, fmt.Errorf("record %q stored under stratum %q declares %q", p.ID, stratum, p.Stratum)
			}
			if p.ID == "" || seen[p.ID] {
				return nil, fmt.Errorf("sampler state contains empty or duplicated retained record ID")
			}
			if want := samplingKey(s.salt, p.ID); want != p.Key {
				return nil, fmt.Errorf("record %q has a sampling key inconsistent with the survey salt", p.ID)
			}
			// Retained records are persisted in ascending key order; equal keys
			// (effectively impossible) are ordered by record ID.
			if i > 0 {
				prev := rs.retained[i-1]
				cur := keyedRecord{record: Record{ID: p.ID}, key: p.Key}
				if !lessKey(prev, cur) {
					return nil, fmt.Errorf("reservoir for stratum %q is not consistently ordered", stratum)
				}
			}
			seen[p.ID] = true
			rs.retained = append(rs.retained, keyedRecord{
				record: Record{ID: p.ID, Stratum: p.Stratum, Amount: p.Amount, AmountText: p.AmountText},
				key:    p.Key,
			})
		}
		filterBits, err := decodeHex(pr.IDFilter)
		if err != nil {
			return nil, fmt.Errorf("stratum %q has corrupt ID filter: %s", stratum, err)
		}
		if len(filterBits) != bloomBits/8 {
			return nil, fmt.Errorf("stratum %q ID filter has unexpected size", stratum)
		}
		s.seenFilters[stratum] = &idFilter{bits: filterBits}
		s.reservoirs[stratum] = rs
	}

	// Only retained IDs are known again after reopening; they re-seed the
	// in-memory overlap guard. Unselected IDs remain unknown and are not stored.
	for id := range seen {
		s.seenIDs[id] = true
	}

	for _, ps := range doc.Sources {
		if ps.ID == "" || ps.Count <= 0 {
			return nil, fmt.Errorf("sampler state contains an invalid source entry")
		}
		if _, dup := s.sources[ps.ID]; dup {
			return nil, fmt.Errorf("sampler state lists source %q twice", ps.ID)
		}
		s.sources[ps.ID] = SourceInfo{ID: ps.ID, Count: ps.Count, Fingerprint: ps.Fingerprint}
	}
	return s, nil
}

// LoadSamplerFile restores a sampler from a state file.
func LoadSamplerFile(path string) (*Sampler, error) {
	if path == "" {
		return nil, fmt.Errorf("empty state file path")
	}
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read sampler state: %s", err)
	}
	return LoadSampler(bytes.NewReader(data))
}

func decodeHex(h string) ([]byte, error) {
	if len(h)%2 != 0 {
		return nil, fmt.Errorf("odd length hex string")
	}
	out := make([]byte, len(h)/2)
	for i := 0; i < len(out); i++ {
		hi, err := hexNibble(h[2*i])
		if err != nil {
			return nil, err
		}
		lo, err := hexNibble(h[2*i+1])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex byte %q", c)
	}
}
