package toml_test

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// A cycle through a map key: x holds p, p points at x, and p is the map's
// key. Marshal must return a cycle error; a hang is the factory's per-test
// timeout.
func TestReproMapKeyCycle(t *testing.T) {
	var x interface{}
	p := &x
	x = p
	m := map[*interface{}]int{p: 1}
	b, err := toml.Marshal(m)
	if err == nil || b != nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want a nil slice and a cycle error, got b=%q err=%v", b, err)
	}
}
