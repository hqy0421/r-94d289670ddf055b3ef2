# Mergeable stratified sampling in Agouti

This document describes the sampling subsystem added alongside Agouti's
WebDriver API. It addresses one specific audit problem: **the few records
scraped from each page must not be naively concatenated into an overall
sample**, because page fragments have different sizes and a record picked from
a page is not a draw from the whole population.

## Data model

- A **record** (`Record`) is one observed population unit, read from a page.
- A **shard** (`Shard`) is one *complete*, mutually-exclusive slice of the
  batch read from a single logged-in page view. Every row the page exposes for
  that source is included; a shard is never a convenience sample.
- Shards come from **non-overlapping sources** (`Shard.Source`). Record IDs are
  globally unique, so an ID appearing in two sources is an overlap and is
  rejected.
- A **sampler** (`Sampler`) is configured once with a batch name, the declared
  strata and each stratum's maximum retained sample size `K`.

## How the sample is drawn: independent sampling keys

Every observed record receives an independent, effectively uniform 64-bit
**sampling key** derived as `SHA-256(surveySalt, recordID)`. For each stratum
the sampler retains the `K` records with the smallest keys (or all records when
fewer than `K` have been observed). Because the keys are independent uniform
draws fixed to the records:

1. The retained set is a simple random sample **without replacement** (SRSWOR)
   of `min(N, K)` from the observed population. This is verified by a
   chi-square goodness-of-fit test over every possible subset.
2. Merging two shards or two samplers is just **keeping the smallest keys
   across both sides** — no new randomness is drawn, and neither side is
   weighted by how many records it happened to retain. After merging, every
   equal-sized subset of the combined population is still equally likely even
   when the two shard sizes differ.

This is why the merge cannot be done by weighting on sample counts: the
contribution of each side is determined by its population through the shared
key ordering, not by the length of its retained slice.

## What is recorded

For each stratum the sampler keeps:

- `N`, the **actual population size** observed across every accepted source;
- at most `K` retained records (the only records ever stored);
- a bit-only Bloom filter of observed record IDs used to detect overlaps after
  a save/reload, without storing the unselected IDs.

The sampler also records every accepted **source** with its observed row count
and a SHA-256 fingerprint of its canonical contents.

## Re-collection, source changes and conflicts

- Re-submitting an **identical** shard for a known source is idempotent: it is
  not re-sampled and the counts do not accumulate.
- A **changed** shard for a known source is refused.
- Overlapping sources (a record ID seen before, even if it was not retained)
  are refused.
- Merging requires the same batch name, survey salt, stratum set and
  per-stratum `K`, and disjoint sources. Any conflict is refused and leaves the
  sampler unmodified.

## Estimates (`Sampler.Report`)

All point estimates weight strata by their actual population `N`, never by
sample count:

- stratum total: `N_h · mean_h`
- overall total: `Σ_h N_h · mean_h`
- overall mean: total / `N`
- finite-population-corrected variance of the total:
  `Σ_h N_h² · (1 − n_h/N_h) · s_h² / n_h`

Special cases:

- **Census** (`n_h = N_h`): the FPC factor is zero, so the stratum's standard
  error is exactly zero.
- **Non-census single sample** (`n_h = 1`, `N_h > 1`): the variance cannot be
  estimated. The report stays valid for point estimates but is explicitly
  marked `Estimable = false` with `StandardError = NaN` and a reason naming the
  stratum.

## Persistence

`Save` / `SaveFile` and `LoadSampler` / `LoadSamplerFile` round-trip the
survey. The persisted document contains only the configuration, the retained
records with their keys, population sizes, source metadata and the bit filters.
It never contains the unselected records. A reopened sampler can continue
collecting, merge with other samplers and produce reports; the stored salt
guarantees reopened and merged samplers derive identical keys.

## Reading pages

`CollectPage` reads a complete shard directly from a logged-in `*Page` by
running one DOM script (selectors are passed as arguments, never concatenated
into the script). `CollectRows` accepts rows extracted through Agouti's
`Selection` API when a script is undesirable. Both feed the same shard
validation and sampling path.
