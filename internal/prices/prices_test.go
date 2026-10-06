package prices

import (
	"errors"
	"testing"
	"time"
)

func TestParseUSDPerMTok(t *testing.T) {
	cases := []struct {
		text    string
		want    USDPerMTok
		wantErr bool
	}{
		{text: "0.20", want: 200000},
		{text: "1.2", want: 1200000},
		{text: "4", want: 4000000},
		{text: "0.000001", want: 1},
		{text: "0.0000001", wantErr: true},
		{text: "-1", wantErr: true},
		{text: "1e3", wantErr: true},
		{text: "abc", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseUSDPerMTok(c.text)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseUSDPerMTok(%q): want error, got %d", c.text, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseUSDPerMTok(%q): unexpected error: %v", c.text, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseUSDPerMTok(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// TestEmbeddedTableParses is the build-breaking guard the package doc
// comment promises: if internal/prices/prices.yml is ever hand-edited into
// something malformed (an unknown key, a bad decimal, a non-positive
// input/output, cached_input > input, a bad as_of), this test -- and so
// `go test`/`make verify` -- fails before the bad edit ever reaches
// `make install`.
func TestEmbeddedTableParses(t *testing.T) {
	if _, err := loadTable(); err != nil {
		t.Fatalf("internal/prices/prices.yml failed to parse/validate: %v", err)
	}
}

func TestLookupKnownModels(t *testing.T) {
	luna, err := Lookup("gpt-5.6-luna")
	if err != nil {
		t.Fatalf("Lookup(gpt-5.6-luna): %v", err)
	}
	if luna.Input != 200000 || luna.CachedInput != 20000 || luna.Output != 1200000 {
		t.Fatalf("gpt-5.6-luna = %+v, want input=200000 cached_input=20000 output=1200000", luna)
	}
	if luna.Source != "https://developers.openai.com/api/docs/pricing" || luna.AsOf != "2026-09-28" {
		t.Fatalf("gpt-5.6-luna source/as_of = %q/%q", luna.Source, luna.AsOf)
	}

	opus, err := Lookup("claude-opus-5-5")
	if err != nil {
		t.Fatalf("Lookup(claude-opus-5-5): %v", err)
	}
	if opus.Input != 4000000 || opus.CacheWrite != 5000000 || opus.CachedInput != 200000 || opus.Output != 20000000 {
		t.Fatalf("claude-opus-5-5 = %+v, want input=4000000 cache_write=5000000 cached_input=200000 output=20000000", opus)
	}
}

// TestLookupMissingModelIsZeroPrice: an id the table lacks prices at $0
// and reports ErrNoPrice, so doctor can warn.
func TestLookupMissingModelIsZeroPrice(t *testing.T) {
	p, err := Lookup("does-not-exist")
	if !errors.Is(err, ErrNoPrice) {
		t.Fatalf("Lookup(does-not-exist) error = %v, want ErrNoPrice", err)
	}
	if p != (ModelPrice{}) {
		t.Fatalf("Lookup(does-not-exist) = %+v, want the zero price", p)
	}
}

// TestLocalModelIsExplicitlyFree: the local model's row is an explicit $0.
func TestLocalModelIsExplicitlyFree(t *testing.T) {
	p, err := Lookup("qwen38-mtplx-quality")
	if err != nil {
		t.Fatalf("Lookup(qwen38-mtplx-quality): %v", err)
	}
	if p.Input != 0 || p.Output != 0 {
		t.Fatalf("qwen38-mtplx-quality = %+v, want input 0 and output 0", p)
	}
}

func TestEffectiveCachedInputAndCacheWriteDefaultToInput(t *testing.T) {
	p := ModelPrice{Input: 1000000, Output: 5000000}
	if got := p.EffectiveCachedInput(); got != 1000000 {
		t.Errorf("EffectiveCachedInput() = %d, want 1000000 (falls back to input)", got)
	}
	if got := p.EffectiveCacheWrite(); got != 1000000 {
		t.Errorf("EffectiveCacheWrite() = %d, want 1000000 (falls back to input)", got)
	}
}

func TestStaleAsOf(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	fresh := ModelPrice{AsOf: "2026-09-01"}
	if stale, reason := fresh.StaleAsOf(now, 90*24*time.Hour); stale {
		t.Errorf("fresh as_of reported stale: %s", reason)
	}
	stalePrice := ModelPrice{AsOf: "2025-01-01"}
	if stale, _ := stalePrice.StaleAsOf(now, 90*24*time.Hour); !stale {
		t.Error("year-old as_of not reported stale")
	}
	missing := ModelPrice{}
	if stale, reason := missing.StaleAsOf(now, 90*24*time.Hour); !stale || reason == "" {
		t.Error("missing as_of not reported stale")
	}
}
