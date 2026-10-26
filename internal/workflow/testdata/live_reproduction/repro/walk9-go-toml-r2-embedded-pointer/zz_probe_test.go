package toml_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func run(t *testing.T, v interface{}) ([]byte, error) {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { b, err := toml.Marshal(v); ch <- res{b, err} }()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("HANG")
		return nil, nil
	}
}

// Cycle through an anonymous embedded pointer.
func TestProbeEmbeddedPointerCycle(t *testing.T) {
	type Node struct {
		*Node
		Value int
	}
	n := &Node{Value: 1}
	n.Node = n
	b, err := run(t, n)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("PROBE2 FAIL: want cycle error, got b=%q err=%v", b, err)
	}
	t.Logf("PROBE2 PASS: %v", err)
}
