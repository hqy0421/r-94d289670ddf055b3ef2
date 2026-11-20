package agouti

import (
	"fmt"
	"strings"
)

// PageShardSpec describes how to read one complete shard from a logged-in
// page. All selectors are CSS selectors.
type PageShardSpec struct {
	// Source identifies the page view the shard is read from (URL, page number
	// or partition key). It becomes the shard's non-overlapping source ID.
	Source string

	// RowSelector selects one element per population row on the page.
	RowSelector string

	// IDSelector, evaluated relative to each row element, selects the element
	// whose text is the record's unique ID. Required.
	IDSelector string

	// AmountSelector, evaluated relative to each row element, selects the
	// element whose text is the record's raw amount. Optional: rows without an
	// amount contribute zero.
	AmountSelector string

	// StratumSelector, evaluated relative to each row element, selects the
	// element whose text names the record's stratum. Optional: when empty,
	// every row is assigned the constant Stratum field.
	StratumSelector string

	// Stratum is the constant stratum for every row when StratumSelector is
	// empty. Exactly one of StratumSelector / Stratum must be provided.
	Stratum string
}

// PageRow is one row read from a page, in the form it was observed. Amount
// holds the raw on-page text and is parsed only when a report is generated.
type PageRow struct {
	ID      string
	Stratum string
	Amount  string
}

// ScriptPage is the page capability CollectPage requires. *Page satisfies it;
// the narrow interface keeps page collection testable without a browser.
type ScriptPage interface {
	RunScript(body string, arguments map[string]interface{}, result interface{}) error
}

type scriptRow struct {
	ID      string `json:"id"`
	Amount  string `json:"amount"`
	Stratum string `json:"stratum"`
}

// CollectPage reads a complete shard straight from a logged-in page and adds
// it to the sampler. The page's DOM is queried in one script call: every
// element matching RowSelector is read in full, so the resulting shard is a
// complete fragment of the batch rather than a handful of picked records.
//
// Re-collecting the same source is idempotent and does not re-sample. Changed
// or overlapping sources are rejected, exactly as with AddShard.
func (s *Sampler) CollectPage(page ScriptPage, spec PageShardSpec) error {
	if page == nil {
		return fmt.Errorf("nil page")
	}
	if err := spec.validate(); err != nil {
		return err
	}

	const body = `
		var rows = document.querySelectorAll(rowSelector);
		return Array.prototype.map.call(rows, function(row) {
			function textOf(selector) {
				if (!selector) { return ""; }
				var el = row.querySelector(selector);
				return el ? el.textContent.replace(/^\\s+|\\s+$/g, "") : "";
			}
			return {
				id: textOf(idSelector),
				amount: textOf(amountSelector),
				stratum: textOf(stratumSelector)
			};
		});
	`

	var raw []scriptRow
	arguments := map[string]interface{}{
		"rowSelector":     spec.RowSelector,
		"idSelector":      spec.IDSelector,
		"amountSelector":  spec.AmountSelector,
		"stratumSelector": spec.StratumSelector,
	}
	if err := page.RunScript(body, arguments, &raw); err != nil {
		return fmt.Errorf("failed to read shard from source %q: %s", spec.Source, err)
	}

	rows := make([]PageRow, 0, len(raw))
	for _, r := range raw {
		stratum := r.Stratum
		if spec.StratumSelector == "" {
			stratum = spec.Stratum
		}
		rows = append(rows, PageRow{ID: r.ID, Stratum: stratum, Amount: r.Amount})
	}
	return s.CollectRows(spec.Source, rows)
}

// CollectRows adds a shard of rows already read from a page (for example with
// Selection.All/At/Text when a script is undesirable). The rows must form one
// complete, mutually-exclusive shard of the named source.
func (s *Sampler) CollectRows(source string, rows []PageRow) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("shard source must not be empty")
	}
	if len(rows) == 0 {
		return fmt.Errorf("source %q yielded no rows", source)
	}
	shard := Shard{Source: source, Records: make([]Record, 0, len(rows))}
	for i, row := range rows {
		if row.Stratum == "" {
			return fmt.Errorf("source %q row %d has no stratum", source, i)
		}
		shard.Records = append(shard.Records, Record{
			ID:         row.ID,
			Stratum:    row.Stratum,
			AmountText: row.Amount,
		})
	}
	return s.AddShard(shard)
}

func (spec PageShardSpec) validate() error {
	if strings.TrimSpace(spec.Source) == "" {
		return fmt.Errorf("page shard spec must name a source")
	}
	if strings.TrimSpace(spec.RowSelector) == "" {
		return fmt.Errorf("page shard spec for source %q requires a RowSelector", spec.Source)
	}
	if strings.TrimSpace(spec.IDSelector) == "" {
		return fmt.Errorf("page shard spec for source %q requires an IDSelector", spec.Source)
	}
	if spec.StratumSelector == "" && strings.TrimSpace(spec.Stratum) == "" {
		return fmt.Errorf("page shard spec for source %q requires StratumSelector or Stratum", spec.Source)
	}
	if spec.StratumSelector != "" && spec.Stratum != "" {
		return fmt.Errorf("page shard spec for source %q must not set both StratumSelector and Stratum", spec.Source)
	}
	return nil
}
