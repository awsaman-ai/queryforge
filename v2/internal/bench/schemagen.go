package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
)

// The scale matrix in section 17 asks for 100, 1,000 and 10,000 schema
// entries. Nothing that large exists to hand, so the harness generates it --
// and HOW it is generated decides what the resulting numbers mean.
//
// Two properties matter more than realism:
//
//  1. The SAME sentences must run at every scale. A latency curve across
//     schema sizes is only readable if the request is held constant, so a
//     generated config is a REAL reviewed config (commerce.json, the Phase 7
//     corpus) with synthetic entities appended around it. The core entities,
//     fields, values and relationships are byte-identical at every scale.
//  2. The padding must not change the core's meaning. Padding entities join
//     only to each other, never to the core, so the schema graph the binder
//     walks from a core root is exactly the graph it walks at 100 entries.
//     If an answer changes at scale, that is retrieval noise from a bigger
//     index -- a finding -- and not the generator moving the goalposts.
//
// Generation is deterministic: the same target always produces the same bytes,
// so a benchmark can be re-run and compared.

// padFieldsPerEntity is the shape of one synthetic entity: an id, a label, an
// amount, a date, an enum with values, a note, and (for every entity after the
// first) a foreign key to its predecessor.
const (
	padEnumValues  = 3
	padEnumAliases = 1
)

// padNouns supply plausible, distinct vocabulary so the synthetic half of the
// corpus is not a thousand copies of one word -- an index of near-identical
// vectors would make retrieval look better under load than it is.
var padNouns = []string{
	"shipment", "invoice", "supplier", "warehouse", "carrier", "contract",
	"ticket", "asset", "campaign", "region", "vendor", "policy", "batch",
	"channel", "device", "facility", "incident", "license", "manifest",
	"partner", "payment", "pipeline", "quota", "receipt", "reservation",
	"route", "session", "shift", "subscription", "tariff", "terminal", "vehicle",
}

// Generated describes one produced config and the counts a report must cite.
type Generated struct {
	Path string `json:"path"`
	// Target is the requested corpus-entry count; Entries is what was reached.
	Target  int `json:"target_entries"`
	Entries int `json:"corpus_entries"`
	// Surfaces is how many texts the surface strategy embeds -- the number of
	// VECTORS in the index, which is what retrieval actually scans.
	Surfaces int `json:"index_vectors"`
	// PadEntities and PadFields say how much was synthesised on top of the base.
	PadEntities int `json:"pad_entities"`
	PadFields   int `json:"pad_extra_fields"`
	// BaseEntries is the reviewed config's own contribution.
	BaseEntries int `json:"base_entries"`
	// ConfigEntities and ConfigFields are the schema-element counts, for readers
	// who read "schema entries" as configured elements rather than index rows.
	ConfigEntities int `json:"config_entities"`
	ConfigFields   int `json:"config_fields"`
}

// Generate writes a config with approximately target corpus entries to
// outPath, built from the reviewed config at basePath.
//
// The count is exact when the target is reachable: padding entities get the
// bulk of the way there and single extra fields close the remainder. A target
// below the base config's own size is an error rather than a silent truncation
// -- the base is never trimmed, because trimming it would change the meaning
// of the sentences the benchmark runs.
func Generate(basePath, outPath string, target int) (Generated, error) {
	baseDoc, err := readJSONObject(basePath)
	if err != nil {
		return Generated{}, err
	}

	baseEntries, _, err := countCorpus(baseDoc)
	if err != nil {
		return Generated{}, fmt.Errorf("base config %s: %w", basePath, err)
	}
	if target < baseEntries {
		return Generated{}, fmt.Errorf("target %d is below the base config's own %d corpus entries; "+
			"the base is never trimmed", target, baseEntries)
	}

	// Each padding entity contributes: itself, its fields, and its enum's
	// values and aliases. The first one has no foreign key, so it is one field
	// (and one relationship) lighter.
	perEntity := 1 + padFieldCount(false) + padEnumValues + padEnumAliases + 1 // +1 relationship
	pad := 0
	if remaining := target - baseEntries; remaining > 0 {
		pad = remaining / perEntity
	}

	// Close the remainder with extra fields (one corpus entry each), then
	// verify against the real corpus builder rather than trusting the formula.
	doc, entries, surfaces, extra, err := fit(baseDoc, target, pad, perEntity)
	if err != nil {
		return Generated{}, err
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return Generated{}, fmt.Errorf("encode generated config: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return Generated{}, err
	}
	if err := os.WriteFile(outPath, encoded, 0o644); err != nil {
		return Generated{}, err
	}

	// A generated config that does not load is worse than no benchmark: it
	// would be measured against a config no real deployment could use.
	cfg, err := config.Load(outPath)
	if err != nil {
		return Generated{}, fmt.Errorf("generated config does not load: %w", err)
	}

	return Generated{
		Path:           outPath,
		Target:         target,
		Entries:        entries,
		Surfaces:       surfaces,
		PadEntities:    pad,
		PadFields:      extra,
		BaseEntries:    baseEntries,
		ConfigEntities: len(cfg.Entities),
		ConfigFields:   len(cfg.Fields),
	}, nil
}

// fit builds the document and adjusts the padding until the corpus lands on
// the target.
//
// It moves in PROPORTIONAL steps rather than one entity at a time: the first
// version walked down by one and ran out of iterations 2 entries short of
// 10,000, silently reporting 9,998. Entities carry the coarse distance and
// single filler fields (one corpus entry each) close the remainder.
func fit(base map[string]any, target, pad, perEntity int) (doc map[string]any, entries, surfaces, extra int, err error) {
	if perEntity < 1 {
		perEntity = 1
	}
	for attempt := 0; attempt < 128; attempt++ {
		doc = buildDoc(base, pad, extra)
		entries, surfaces, err = countCorpus(doc)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		gap := target - entries
		switch {
		case gap == 0:
			return doc, entries, surfaces, extra, nil

		case gap > 0:
			// Whole entities first, so a large shortfall is not closed with a
			// thousand filler fields hanging off one entity.
			if gap >= perEntity {
				pad += gap / perEntity
				continue
			}
			extra += gap

		default:
			// Overshoot: drop the filler first, then whole entities.
			if extra > 0 {
				if -gap <= extra {
					extra += gap
					continue
				}
				gap += extra
				extra = 0
			}
			if pad == 0 {
				return doc, entries, surfaces, extra, nil
			}
			step := (-gap)/perEntity + 1
			if step > pad {
				step = pad
			}
			pad -= step
		}
	}
	return doc, entries, surfaces, extra, nil
}

// buildDoc appends pad synthetic entities (plus extra filler fields on the
// last of them) to a copy of the base document.
func buildDoc(base map[string]any, pad, extra int) map[string]any {
	doc := map[string]any{}
	for k, v := range base {
		doc[k] = v
	}

	entities := append([]any{}, asSlice(base["entities"])...)
	fields := append([]any{}, asSlice(base["fields"])...)
	relationships := append([]any{}, asSlice(base["relationships"])...)

	for i := 0; i < pad; i++ {
		name := padEntityName(i)
		entities = append(entities, map[string]any{
			"name":        name,
			"description": fmt.Sprintf("Synthetic scale-test entity %d, generated for the Phase 9 benchmark.", i),
			"synonyms":    []any{name, fmt.Sprintf("%s record", name)},
			"table":       name + "_rows",
		})
		fields = append(fields, padFields(name, i, i > 0)...)
		if i > 0 {
			prev := padEntityName(i - 1)
			relationships = append(relationships, map[string]any{
				"name":        fmt.Sprintf("%s_%s", prev, name),
				"description": fmt.Sprintf("Synthetic link from %s to %s.", prev, name),
				"synonyms":    []any{fmt.Sprintf("linked %s", name)},
				"from":        map[string]any{"entity": prev, "field": "id"},
				"to":          map[string]any{"entity": name, "field": "parent_id"},
				"cardinality": "one-to-many",
			})
		}
	}

	// Filler fields land on the last synthetic entity, or on the first base
	// entity when there is no padding at all (a target just above the base).
	if extra > 0 {
		owner := padEntityName(pad - 1)
		if pad == 0 {
			owner = firstEntityName(base)
		}
		for i := 0; i < extra; i++ {
			fields = append(fields, map[string]any{
				"entity":      owner,
				"name":        fmt.Sprintf("filler_%03d", i),
				"type":        "number",
				"description": fmt.Sprintf("Synthetic filler metric %d.", i),
				"synonyms":    []any{fmt.Sprintf("filler metric %d", i)},
				"selectable":  false,
			})
		}
	}

	doc["entities"] = entities
	doc["fields"] = fields
	doc["relationships"] = relationships
	return doc
}

func padFieldCount(withForeignKey bool) int {
	n := 6 // id, label, amount, opened_at, state, note
	if withForeignKey {
		n++
	}
	return n
}

func padFields(entity string, i int, withForeignKey bool) []any {
	noun := padNouns[i%len(padNouns)]
	out := []any{
		map[string]any{
			"entity": entity, "name": "id", "type": "number",
			"description": fmt.Sprintf("Unique identifier of the %s.", noun),
			"synonyms":    []any{fmt.Sprintf("%s id", noun)},
			"selectable":  false,
		},
		map[string]any{
			"entity": entity, "name": "label", "type": "string",
			"description": fmt.Sprintf("Human-readable label of the %s.", noun),
			"synonyms":    []any{fmt.Sprintf("%s label", noun), fmt.Sprintf("%s title", noun)},
		},
		map[string]any{
			"entity": entity, "name": "amount", "type": "number",
			"description": fmt.Sprintf("Monetary amount recorded on the %s.", noun),
			"synonyms":    []any{fmt.Sprintf("%s amount", noun), fmt.Sprintf("%s value", noun)},
		},
		map[string]any{
			"entity": entity, "name": "opened_at", "type": "date",
			"description": fmt.Sprintf("Date the %s was opened.", noun),
			"synonyms":    []any{fmt.Sprintf("%s opened date", noun)},
		},
		map[string]any{
			"entity": entity, "name": "state", "type": "enum",
			"description":  fmt.Sprintf("Lifecycle state of the %s.", noun),
			"synonyms":     []any{fmt.Sprintf("%s state", noun)},
			"enumValues":   []any{"open", "closed", "pending"},
			"valueAliases": map[string]any{fmt.Sprintf("%s finished", noun): "closed"},
		},
		map[string]any{
			"entity": entity, "name": "note", "type": "string",
			"description": fmt.Sprintf("Free-text note attached to the %s.", noun),
			"synonyms":    []any{fmt.Sprintf("%s note", noun)},
		},
	}
	if withForeignKey {
		out = append(out, map[string]any{
			"entity": entity, "name": "parent_id", "type": "number",
			"description": fmt.Sprintf("Identifier of the %s this one belongs to.", noun),
			"synonyms":    []any{fmt.Sprintf("%s parent id", noun)},
			"selectable":  false,
			"filterable":  false,
		})
	}
	return out
}

// padEntityName is stable for a given index, which is what makes two runs of
// the generator produce identical bytes.
func padEntityName(i int) string {
	return fmt.Sprintf("%s_%04d", padNouns[i%len(padNouns)], i)
}

// countCorpus runs the real decoder and the real corpus builder, so the number
// in the report is the number the engine will index -- not an estimate.
func countCorpus(doc map[string]any) (entries, surfaces int, err error) {
	encoded, err := json.Marshal(doc)
	if err != nil {
		return 0, 0, err
	}
	cfg, err := config.Decode(bytes.NewReader(encoded))
	if err != nil {
		return 0, 0, err
	}
	built := corpus.Build(config.NewSchemaRegistry(cfg))
	for _, e := range built {
		surfaces += len(e.Surfaces)
	}
	return len(built), surfaces, nil
}

func readJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func firstEntityName(doc map[string]any) string {
	for _, e := range asSlice(doc["entities"]) {
		if m, ok := e.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				return name
			}
		}
	}
	return ""
}
