package retrieval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
)

func saveTo(t *testing.T, ix *Index) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.index.json")
	if err := ix.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

// Index build and reload, from the brief's test list. A reloaded index must be
// indistinguishable from the one that was built, or the cache changes answers.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	for _, strategy := range Strategies() {
		t.Run(string(strategy), func(t *testing.T) {
			entries := testCorpus(t, "commerce.json")
			built, provider := buildIndex(t, "commerce.json", strategy)
			path := saveTo(t, built)

			loaded, err := Load(path, entries)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if loaded.Len() != built.Len() || loaded.VectorCount() != built.VectorCount() {
				t.Errorf("shape differs: %d/%d entries, %d/%d vectors",
					loaded.Len(), built.Len(), loaded.VectorCount(), built.VectorCount())
			}
			if loaded.Strategy() != built.Strategy() {
				t.Errorf("strategy = %q, want %q", loaded.Strategy(), built.Strategy())
			}
			if loaded.Identity() != built.Identity() {
				t.Errorf("identity = %v, want %v", loaded.Identity(), built.Identity())
			}

			// The real test: the same query must produce the same ranking.
			// Float32 survives the base64 round trip exactly, so the scores
			// should match bit for bit, not merely closely.
			q, err := provider.EmbedOne(context.Background(), "customer spending")
			if err != nil {
				t.Fatalf("EmbedOne: %v", err)
			}
			before, err := built.Search(q, 10)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			after, err := loaded.Search(q, 10)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(before) != len(after) {
				t.Fatalf("result count differs: %d then %d", len(before), len(after))
			}
			for i := range before {
				if before[i].Entry.ID != after[i].Entry.ID {
					t.Errorf("result %d: %s before, %s after reload", i, before[i].Entry.ID, after[i].Entry.ID)
				}
				if before[i].Score != after[i].Score {
					t.Errorf("result %d score: %v before, %v after reload", i, before[i].Score, after[i].Score)
				}
			}
		})
	}
}

// Cache reuse, from the brief's test list: the whole point of persisting is not
// paying the embedding cost again.
func TestLoadDoesNotEmbedAnything(t *testing.T) {
	entries := testCorpus(t, "commerce.json")
	built, _ := buildIndex(t, "commerce.json", StrategySurface)
	path := saveTo(t, built)

	// Load takes no provider at all, which is the structural guarantee. This
	// test states the property so that adding one later is a visible decision.
	loaded, err := Load(path, entries)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.VectorCount() != built.VectorCount() {
		t.Errorf("reloaded index has %d vectors, want %d", loaded.VectorCount(), built.VectorCount())
	}
}

// A stale index is more dangerous than a corrupt one: it parses cleanly and
// retrieves confidently under names the config no longer uses.
func TestLoadRejectsStaleIndex(t *testing.T) {
	built, _ := buildIndex(t, "commerce.json", StrategySurface)
	path := saveTo(t, built)

	_, err := Load(path, testCorpus(t, "customers.json"))
	if err == nil {
		t.Fatal("an index built from a different config was accepted")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error = %v, should say the index is stale", err)
	}
}

// Corrupt index rejection, from the brief's test list. Each case damages the
// file a different way; none may load.
func TestLoadRejectsCorruptIndex(t *testing.T) {
	entries := testCorpus(t, "commerce.json")
	built, _ := buildIndex(t, "commerce.json", StrategySurface)

	cases := []struct {
		name    string
		damage  func(map[string]any)
		wantMsg string
	}{
		{"wrong format version", func(m map[string]any) { m["format"] = 99 }, "format"},
		{"unknown strategy", func(m map[string]any) { m["strategy"] = "telepathy" }, "strategy"},
		{"zero dimension", func(m map[string]any) {
			m["identity"].(map[string]any)["dim"] = 0
		}, "dimension"},
		{"dim disagrees with identity", func(m map[string]any) { m["dim"] = 7 }, "inconsistent"},
		{"truncated vector data", func(m map[string]any) {
			s := m["vectors"].(string)
			m["vectors"] = s[:len(s)/2]
		}, "corrupt"},
		{"vector data is not base64", func(m map[string]any) { m["vectors"] = "!!!not base64!!!" }, "base64"},
		{"no vectors", func(m map[string]any) { m["count"] = 0 }, "corrupt"},
		{"owner points past the entries", func(m map[string]any) {
			owners := m["owners"].([]any)
			owners[0].(map[string]any)["e"] = 99999
		}, "points at entry"},
		{"owner points past the surfaces", func(m map[string]any) {
			owners := m["owners"].([]any)
			owners[0].(map[string]any)["s"] = 99999
		}, "surface"},
		{"owner count disagrees with vectors", func(m map[string]any) {
			owners := m["owners"].([]any)
			m["owners"] = owners[:len(owners)-1]
		}, "owner records"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := saveTo(t, built)

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading index: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("index is not valid JSON: %v", err)
			}
			tc.damage(m)

			damaged, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("re-encoding: %v", err)
			}
			if err := os.WriteFile(path, damaged, 0o644); err != nil {
				t.Fatalf("writing index: %v", err)
			}

			_, err = Load(path, entries)
			if err == nil {
				t.Fatal("a corrupt index was accepted")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantMsg)) {
				t.Errorf("error = %v, should mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestLoadRejectsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	entries := testCorpus(t, "customers.json")

	cases := map[string]string{
		"not json":         "this is not an index",
		"truncated json":   `{"format":1,"identity":{`,
		"trailing content": `{"format":1,"identity":{"dim":4},"strategy":"passage","dim":4,"count":1,"vectors":"AAAAAA=="}{"extra":true}`,
		"empty":            "",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("writing: %v", err)
			}
			if _, err := Load(path, entries); err == nil {
				t.Fatal("a malformed index file was accepted")
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"), testCorpus(t, "customers.json"))
	if err == nil {
		t.Fatal("a missing index file was accepted")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, should say the file was not found", err)
	}
}

// Passing a nil corpus skips the staleness check, for tooling that wants to
// inspect an index without the config that produced it.
func TestLoadWithoutCorpusSkipsStalenessCheck(t *testing.T) {
	built, _ := buildIndex(t, "commerce.json", StrategyPassage)
	path := saveTo(t, built)

	if _, err := Load(path, nil); err != nil {
		t.Fatalf("Load with a nil corpus should succeed: %v", err)
	}
}

// An interrupted write must not destroy the previous index.
func TestSaveIsAtomic(t *testing.T) {
	built, _ := buildIndex(t, "commerce.json", StrategyPassage)
	path := saveTo(t, built)

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file should not survive a successful save")
	}

	// Saving again over a good file must leave a good file.
	if err := built.Save(path); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if _, err := Load(path, testCorpus(t, "commerce.json")); err != nil {
		t.Fatalf("index is unusable after being overwritten: %v", err)
	}
}

func TestSaveCreatesParentDirectories(t *testing.T) {
	built, _ := buildIndex(t, "customers.json", StrategyPassage)
	path := filepath.Join(t.TempDir(), "nested", "deeper", "corpus.index.json")
	if err := built.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index was not written: %v", err)
	}
}

// The index holds config VALUES — enum values and aliases are embedded, and
// their text is stored so results can be explained. That is expected; what
// matters is that they stay marked, so whatever renders them redacts by
// default.
func TestPersistedValuesStayMarkedSensitive(t *testing.T) {
	entries := testCorpus(t, "commerce.json")
	built, _ := buildIndex(t, "commerce.json", StrategySurface)
	path := saveTo(t, built)

	loaded, err := Load(path, entries)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var checked int
	for _, e := range loaded.Entries() {
		if e.Kind != corpus.KindValueAlias {
			continue
		}
		checked++
		for _, s := range e.Surfaces {
			if !s.Sensitive {
				t.Errorf("entry %s lost its sensitive marker across the round trip", e.ID)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no value entries survived the round trip")
	}
}

var _ embedding.Provider = (*embedding.Deterministic)(nil)
