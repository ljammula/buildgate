package toml_test

import (
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// Acyclic: S.Ref points at S's first field, so the root *S and Ref's *int
// share an address. The value has no cycle and must encode as before.
func TestReproFirstFieldPointerIsNotACycle(t *testing.T) {
	type S struct {
		Value int
		Ref   *int
	}
	s := &S{Value: 1}
	s.Ref = &s.Value
	b, err := toml.Marshal(s)
	if err != nil {
		t.Fatalf("acyclic value rejected: %v", err)
	}
	t.Logf("%q", b)
}
