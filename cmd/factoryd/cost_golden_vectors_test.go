package main

import (
	"encoding/json"
	"os"
	"testing"
)

// TestFormatTokenCountMatchesTheConsoleGoldenVectors reads the file the
// console's own tests read (console/test/request_cost_test.dart), so a
// token count reads the same in `factoryd status`, a round summary and the
// console.
func TestFormatTokenCountMatchesTheConsoleGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile("../../console/test/fixtures/vectors/cost.json")
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var vectors struct {
		FormatTokenCount []struct {
			N    int64  `json:"n"`
			Want string `json:"want"`
		} `json:"format_token_count"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode golden vectors: %v", err)
	}
	if len(vectors.FormatTokenCount) == 0 {
		t.Fatal("golden vectors have no format_token_count section")
	}
	for _, v := range vectors.FormatTokenCount {
		if got := formatTokenCount(v.N); got != v.Want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", v.N, got, v.Want)
		}
	}
}
