package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"queryforge.local/v2experiment/internal/qferr"
)

// Load reads, normalizes and validates a config file.
//
// The brief names a "ConfigLoader" and a "ConfigValidator" (section 9). In Go
// those are functions rather than types: neither holds state between calls, and
// a config.ConfigLoader would stutter at every call site. Load is the loader,
// Validate is the validator, and NewSchemaRegistry is the registry constructor.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, qferr.Wrap(qferr.CodeConfigError, err, "config file not found: %s", path)
		}
		return nil, qferr.Wrap(qferr.CodeConfigError, err, "config file could not be read: %s", path)
	}

	cfg, err := Decode(bytes.NewReader(data))
	if err != nil {
		// Decode's message says "config", not which file; at this level the
		// path is known and worth adding.
		var coded *qferr.Error
		if errors.As(err, &coded) {
			return nil, qferr.Wrap(coded.Code, coded.Wrapped, "%s: %s", path, coded.Message)
		}
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Decode parses config JSON and applies defaults, without validating.
//
// The split exists so tests can construct a deliberately broken config and
// assert on what Validate says about it, rather than having every invalid case
// need a file on disk.
func Decode(r io.Reader) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(r)

	// Unknown fields are tolerated rather than rejected. The config schema gains
	// keys in most later phases, and fixtures are shared across phases: a strict
	// decoder would make a Phase 5 fixture unreadable by a Phase 2 binary, which
	// costs more than it catches. A typo'd key is caught in practice by the
	// missing-value rules — a misspelled "synonym" leaves synonyms empty.
	if err := dec.Decode(&cfg); err != nil {
		return nil, qferr.Wrap(qferr.CodeConfigError, err, "config is not valid JSON")
	}

	// Trailing content usually means two JSON documents in one file, or a stray
	// brace. json.Decoder stops at the end of the first value and would
	// otherwise ignore the rest silently.
	if err := trailingContent(dec); err != nil {
		return nil, err
	}

	cfg.Normalize()
	return &cfg, nil
}

func trailingContent(dec *json.Decoder) error {
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return qferr.Wrap(qferr.CodeConfigError, err, "config is not valid JSON")
	}
	return qferr.New(qferr.CodeConfigError, "config contains more than one JSON document")
}

// Normalize fills in the defaults that validation then checks.
//
// Order matters: Table and Column default to the entity or field name, and the
// identifier rules run afterwards. That is what makes "a name that is not a
// legal identifier must map its table explicitly" a real rule rather than a
// suggestion — the default is validated exactly like an explicit value.
func (c *Config) Normalize() {
	for i := range c.Entities {
		e := &c.Entities[i]
		e.Name = strings.TrimSpace(e.Name)
		if strings.TrimSpace(e.Table) == "" {
			e.Table = e.Name
		}
	}
	for i := range c.Fields {
		f := &c.Fields[i]
		f.Name = strings.TrimSpace(f.Name)
		f.Entity = strings.TrimSpace(f.Entity)
		if strings.TrimSpace(f.Column) == "" {
			f.Column = f.Name
		}
	}
	c.Security.applyDefaults()
}

// applyDefaults substitutes DefaultSecurityPolicy for any limit left at zero.
// Zero means "unset", so a config can override one limit without having to
// restate the rest — and a config with no security block is still bounded.
func (p *SecurityPolicy) applyDefaults() {
	d := DefaultSecurityPolicy
	if p.MaxInputLength == 0 {
		p.MaxInputLength = d.MaxInputLength
	}
	if p.MaxPhrases == 0 {
		p.MaxPhrases = d.MaxPhrases
	}
	if p.MaxCandidates == 0 {
		p.MaxCandidates = d.MaxCandidates
	}
	if p.MaxJoins == 0 {
		p.MaxJoins = d.MaxJoins
	}
	if p.MaxFilters == 0 {
		p.MaxFilters = d.MaxFilters
	}
	if p.MaxNestingDepth == 0 {
		p.MaxNestingDepth = d.MaxNestingDepth
	}
	if p.MaxQueryLength == 0 {
		p.MaxQueryLength = d.MaxQueryLength
	}
	if p.MaxPlanningMillis == 0 {
		p.MaxPlanningMillis = d.MaxPlanningMillis
	}
}

// Summary is a one-line, secret-free description of what was loaded. It backs
// --validate-config, which must prove the config parsed without echoing table
// names, value aliases or enum values.
func (c *Config) Summary() string {
	// Structured rather than prose, which sidesteps pluralisation and matches
	// the field-per-value shape the observability section asks debug output to
	// take (section 24).
	return fmt.Sprintf("entities=%d fields=%d operators=%d aggregations=%d relationships=%d patterns=%d",
		len(c.Entities), len(c.Fields), len(c.OperatorPhrases),
		len(c.AggregationPhrases), len(c.Relationships), len(c.QueryPatterns))
}
