package greet

import "testing"

func TestBanner(t *testing.T) {
	if got := Banner(); got != "BUILDGATE!" {
		t.Errorf("Banner() = %q, want %q", got, "BUILDGATE!")
	}
}
