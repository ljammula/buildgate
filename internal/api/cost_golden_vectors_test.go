package api

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// TestCostGoldenVectorsUseTheAPICostSummaryShape decodes every cost_summary
// the console's usage-line vectors are rendered from
// (console/src/domain/usage.test.ts) into CostSummary with unknown
// fields refused: a vector cannot exercise a field the API does not send.
func TestCostGoldenVectorsUseTheAPICostSummaryShape(t *testing.T) {
	raw, err := os.ReadFile("../../console/test/fixtures/vectors/cost.json")
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var vectors struct {
		Usage []struct {
			Name        string          `json:"name"`
			CostSummary json.RawMessage `json:"cost_summary"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode golden vectors: %v", err)
	}
	if len(vectors.Usage) == 0 {
		t.Fatal("golden vectors have no usage section")
	}
	for _, v := range vectors.Usage {
		decoder := json.NewDecoder(bytes.NewReader(v.CostSummary))
		decoder.DisallowUnknownFields()
		var summary CostSummary
		if err := decoder.Decode(&summary); err != nil {
			t.Errorf("%s: cost_summary is not the API's CostSummary: %v", v.Name, err)
		}
	}
}
