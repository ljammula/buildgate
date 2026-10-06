// Package prices is buildgate's single, compiled-in source of real
// per-model provider prices: internal/prices/prices.yml, embedded at build
// time and parsed once. This replaced a per-model/session-config price
// (2026-09-28 operator decision): a real provider price is a fact about the
// provider, not a per-run choice, and the earlier "3/15 micro-USD per
// token" session-config default was not any real model's price and could
// not express a real one (OpenAI gpt-5.6-luna is $0.20 input / $0.02 cached
// input / $1.20 output per 1M tokens -- 0.2 micro-USD per token, an
// impossible input to an int64-per-token knob) or a cached-input discount
// at all. Editing a price now means editing prices.yml and running
// `make install`, exactly like editing any other compiled-in table.
package prices

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed prices.yml
var pricesYAML []byte

// USDPerMTok is a price in micro-USD per 1,000,000 tokens. It is always
// parsed from the YAML scalar's own TEXT (see UnmarshalYAML), never a
// float64, so a real provider price with up to 6 decimal places of USD
// (e.g. "0.20") round-trips to an exact integer (200000) with no binary-
// floating-point rounding.
type USDPerMTok int64

// decimalPrice matches digits, an optional single '.', and at most 6
// fractional digits -- no sign, no exponent, no leading/trailing junk.
// Anything else (a negative number, "1e3", "abc") is refused.
var decimalPrice = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,6})?$`)

// ParseUSDPerMTok parses text (USD per 1M tokens, e.g. "0.20", "4", "1.2")
// into its exact micro-USD-per-1M-tokens integer value, or an error naming
// text when it is not a plain non-negative decimal with at most 6
// fractional digits.
func ParseUSDPerMTok(text string) (USDPerMTok, error) {
	if !decimalPrice.MatchString(text) {
		return 0, fmt.Errorf("price %q: must be a non-negative decimal number with at most 6 fractional digits, in USD per 1M tokens (e.g. \"0.20\")", text)
	}
	whole, frac, _ := strings.Cut(text, ".")
	for len(frac) < 6 {
		frac += "0"
	}
	wholeVal, err := strconv.ParseInt(whole, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("price %q: %w", text, err)
	}
	fracVal, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("price %q: %w", text, err)
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if wholeVal > (maxInt64-fracVal)/1_000_000 {
		return 0, fmt.Errorf("price %q: out of range", text)
	}
	return USDPerMTok(wholeVal*1_000_000 + fracVal), nil
}

// UnmarshalYAML decodes the scalar's own text via ParseUSDPerMTok -- never
// yaml.v3's own float64 decoding, which is exactly the imprecise path this
// type exists to avoid.
func (u *USDPerMTok) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err != nil {
		return fmt.Errorf("price: %w", err)
	}
	v, err := ParseUSDPerMTok(text)
	if err != nil {
		return err
	}
	*u = v
	return nil
}

// ModelPrice is one provider model's compiled-in price: input, output, and
// the two optional cache rates. Source/AsOf are metadata only (never
// consulted by cost computation), read by `factoryd doctor` to warn on a
// stale or missing as_of.
type ModelPrice struct {
	Input       USDPerMTok `yaml:"input"`
	CachedInput USDPerMTok `yaml:"cached_input,omitempty"`
	CacheWrite  USDPerMTok `yaml:"cache_write,omitempty"`
	Output      USDPerMTok `yaml:"output"`
	Source      string     `yaml:"source,omitempty"`
	AsOf        string     `yaml:"as_of,omitempty"`
}

// EffectiveCachedInput is CachedInput, defaulting to Input when the table
// entry omits it (no cache discount assumed).
func (p ModelPrice) EffectiveCachedInput() USDPerMTok {
	if p.CachedInput == 0 {
		return p.Input
	}
	return p.CachedInput
}

// EffectiveCacheWrite is CacheWrite, defaulting to Input when the table
// entry omits it (no cache-write premium assumed).
func (p ModelPrice) EffectiveCacheWrite() USDPerMTok {
	if p.CacheWrite == 0 {
		return p.Input
	}
	return p.CacheWrite
}

// StaleAsOf reports whether p's AsOf is missing, unparseable, or more than
// staleAfter old as of now -- `factoryd doctor`'s own warning threshold
// (90 days) is passed in by the caller, not hardcoded here, so a test can
// use a different one.
func (p ModelPrice) StaleAsOf(now time.Time, staleAfter time.Duration) (stale bool, reason string) {
	if p.AsOf == "" {
		return true, "no as_of date"
	}
	t, err := time.Parse("2006-01-02", p.AsOf)
	if err != nil {
		return true, fmt.Sprintf("as_of %q is not YYYY-MM-DD", p.AsOf)
	}
	if now.Sub(t) > staleAfter {
		return true, fmt.Sprintf("as_of %s is more than %s old", p.AsOf, staleAfter)
	}
	return false, ""
}

var (
	tableOnce sync.Once
	table     map[string]ModelPrice
	tableErr  error
)

// loadTable parses and validates the embedded prices.yml exactly once,
// strictly (unknown keys refused): a malformed table is a build-breaking
// mistake, not a runtime-tolerated one, so every caller (Lookup, and the
// test that asserts this package's own table parses) shares this single
// parse/validate pass and its single error.
func loadTable() (map[string]ModelPrice, error) {
	tableOnce.Do(func() {
		dec := yaml.NewDecoder(bytes.NewReader(pricesYAML))
		dec.KnownFields(true)
		var raw map[string]ModelPrice
		if err := dec.Decode(&raw); err != nil {
			tableErr = fmt.Errorf("internal/prices/prices.yml: %w", err)
			return
		}
		for id, p := range raw {
			if err := validate(id, p); err != nil {
				tableErr = err
				return
			}
		}
		table = raw
	})
	return table, tableErr
}

func validate(id string, p ModelPrice) error {
	if p.CachedInput < 0 {
		return fmt.Errorf("internal/prices/prices.yml: %s: cached_input must not be negative", id)
	}
	if p.CachedInput > p.Input {
		return fmt.Errorf("internal/prices/prices.yml: %s: cached_input must not exceed input", id)
	}
	if p.CacheWrite < 0 {
		return fmt.Errorf("internal/prices/prices.yml: %s: cache_write must not be negative", id)
	}
	if p.AsOf != "" {
		if _, err := time.Parse("2006-01-02", p.AsOf); err != nil {
			return fmt.Errorf("internal/prices/prices.yml: %s: as_of %q is not YYYY-MM-DD", id, p.AsOf)
		}
	}
	return nil
}

// ErrNoPrice reports a model id with no table entry. Lookup still returns
// a zero price with it: an unpriced model costs $0, so its dollar budget
// and ceiling never trip (its token budget and ceiling still bound it), and
// `factoryd doctor` warns so the operator can add the provider's price.
var ErrNoPrice = errors.New("no price in internal/prices/prices.yml")

// Lookup returns the compiled price for provider model id (models.<m>.id in
// a session config). For an id the table lacks it returns a zero price and
// an error wrapping ErrNoPrice; any other error means the table itself is
// malformed.
func Lookup(id string) (ModelPrice, error) {
	t, err := loadTable()
	if err != nil {
		return ModelPrice{}, err
	}
	p, ok := t[id]
	if !ok {
		return ModelPrice{}, fmt.Errorf("model id %q: %w -- costs show as $0 until you add its provider price (USD per 1M tokens) and rebuild", id, ErrNoPrice)
	}
	return p, nil
}
