package retrieval

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/qferr"
)

// An index is a cache, and the cache strategy in section 10 is the point:
// config → corpus → embeddings → index → ready, computed at startup and NOT
// recomputed per request. Persisting it takes that one step further, so a
// restart does not pay the embedding cost again.
//
// Everything about loading one is therefore defensive. A stale or corrupt index
// does not fail loudly on its own — it retrieves confidently and wrongly, which
// is the worst failure mode available to this system.

// indexFormatVersion guards against silently misreading a file written by an
// older layout. Bumped whenever the on-disk shape changes.
const indexFormatVersion = 1

// persistedIndex is the on-disk form.
type persistedIndex struct {
	Format   int                `json:"format"`
	Identity embedding.Identity `json:"identity"`
	Strategy Strategy           `json:"strategy"`
	// CorpusHash is checked against the live config's corpus on load.
	CorpusHash string `json:"corpusHash"`

	Entries []corpus.Entry `json:"entries"`
	Owners  []ownerJSON    `json:"owners"`

	// Vectors are base64 little-endian float32, not a JSON array of numbers.
	// At 384 dimensions a JSON array costs roughly ten bytes per component, so
	// the 10,000-entry schema in the Phase 9 benchmark matrix would produce a
	// file of tens of megabytes that is slow to parse and no more readable for
	// it. Dim and Count stay in plain fields so the file is still inspectable.
	Vectors string `json:"vectors"`
	Dim     int    `json:"dim"`
	Count   int    `json:"count"`
}

type ownerJSON struct {
	Entry   int `json:"e"`
	Surface int `json:"s"`
}

// Save writes the index to path, creating parent directories as needed.
func (ix *Index) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return qferr.Wrap(qferr.CodeInternal, err, "index directory could not be created")
		}
	}

	owners := make([]ownerJSON, len(ix.owners))
	for i, o := range ix.owners {
		owners[i] = ownerJSON{Entry: o.entry, Surface: o.surface}
	}

	payload := persistedIndex{
		Format:     indexFormatVersion,
		Identity:   ix.identity,
		Strategy:   ix.strategy,
		CorpusHash: ix.corpusHash,
		Entries:    ix.entries,
		Owners:     owners,
		Vectors:    encodeVectors(ix.vectors),
		Dim:        ix.identity.Dim,
		Count:      len(ix.vectors),
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "index could not be encoded")
	}

	// Written to a temporary file and renamed, so an interrupted write leaves
	// the previous index intact rather than a truncated file that loads as far
	// as it goes.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "index could not be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return qferr.Wrap(qferr.CodeInternal, err, "index could not be moved into place")
	}
	return nil
}

// Load reads an index and validates it against the corpus it will be used with.
//
// Passing the live corpus is required, not optional. An index is only valid for
// the config it was built from, and the alternative — loading it blind and
// hoping — is how a renamed field ends up answering questions under its old
// name.
func Load(path string, entries []corpus.Entry) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, qferr.Wrap(qferr.CodeInvalidInput, err, "index file not found: %s", path)
		}
		return nil, qferr.Wrap(qferr.CodeInternal, err, "index file could not be read: %s", path)
	}

	var payload persistedIndex
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&payload); err != nil {
		return nil, qferr.Wrap(qferr.CodeInternal, err, "index file is corrupt: not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, qferr.New(qferr.CodeInternal, "index file is corrupt: trailing content after the index")
	}

	if payload.Format != indexFormatVersion {
		return nil, qferr.New(qferr.CodeInternal,
			"index was written in format %d, this build reads format %d; rebuild it",
			payload.Format, indexFormatVersion)
	}
	if !ValidStrategy(payload.Strategy) {
		return nil, qferr.New(qferr.CodeInternal, "index declares unknown strategy %q", payload.Strategy)
	}
	if payload.Identity.Dim <= 0 {
		return nil, qferr.New(qferr.CodeInternal, "index declares a non-positive dimension (%d)", payload.Identity.Dim)
	}
	if payload.Dim != payload.Identity.Dim {
		return nil, qferr.New(qferr.CodeInternal,
			"index is inconsistent: vector width %d does not match the model's %d",
			payload.Dim, payload.Identity.Dim)
	}

	vectors, err := decodeVectors(payload.Vectors, payload.Dim, payload.Count)
	if err != nil {
		return nil, err
	}
	if len(payload.Owners) != len(vectors) {
		return nil, qferr.New(qferr.CodeInternal,
			"index is corrupt: %d vectors but %d owner records", len(vectors), len(payload.Owners))
	}

	owners := make([]owner, len(payload.Owners))
	for i, o := range payload.Owners {
		if o.Entry < 0 || o.Entry >= len(payload.Entries) {
			return nil, qferr.New(qferr.CodeInternal,
				"index is corrupt: vector %d points at entry %d of %d", i, o.Entry, len(payload.Entries))
		}
		if o.Surface >= 0 && o.Surface >= len(payload.Entries[o.Entry].Surfaces) {
			return nil, qferr.New(qferr.CodeInternal,
				"index is corrupt: vector %d points at surface %d of an entry with %d",
				i, o.Surface, len(payload.Entries[o.Entry].Surfaces))
		}
		owners[i] = owner{entry: o.Entry, surface: o.Surface}
	}

	// The staleness check. Everything above catches a damaged file; this
	// catches an intact file that describes a config which has since changed.
	if entries != nil {
		if live := Fingerprint(entries); live != payload.CorpusHash {
			return nil, qferr.New(qferr.CodeConfigError,
				"index is stale: it was built from a different config (rebuild it)")
		}
	}

	return &Index{
		identity:   payload.Identity,
		strategy:   payload.Strategy,
		corpusHash: payload.CorpusHash,
		entries:    payload.Entries,
		vectors:    vectors,
		owners:     owners,
	}, nil
}

// encodeVectors packs vectors as base64 little-endian float32.
func encodeVectors(vectors [][]float32) string {
	if len(vectors) == 0 {
		return ""
	}
	buf := make([]byte, 0, len(vectors)*len(vectors[0])*4)
	scratch := make([]byte, 4)
	for _, v := range vectors {
		for _, x := range v {
			binary.LittleEndian.PutUint32(scratch, math.Float32bits(x))
			buf = append(buf, scratch...)
		}
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// decodeVectors unpacks what encodeVectors wrote, checking that the byte count
// matches the declared shape. A truncated file fails here rather than producing
// a short final vector that would quietly score against garbage.
func decodeVectors(encoded string, dim, count int) ([][]float32, error) {
	if count == 0 {
		return nil, qferr.New(qferr.CodeInternal, "index is corrupt: it contains no vectors")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, qferr.Wrap(qferr.CodeInternal, err, "index is corrupt: vector data is not valid base64")
	}
	want := dim * count * 4
	if len(raw) != want {
		return nil, qferr.New(qferr.CodeInternal,
			"index is corrupt: vector data is %d bytes, expected %d for %d vectors of %d dimensions",
			len(raw), want, count, dim)
	}

	vectors := make([][]float32, count)
	for i := 0; i < count; i++ {
		v := make([]float32, dim)
		for j := 0; j < dim; j++ {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[(i*dim+j)*4:]))
		}
		vectors[i] = v
	}
	return vectors, nil
}
