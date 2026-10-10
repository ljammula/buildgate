package hostcontrol

import (
	"strings"
	"testing"
)

func TestColimaProfileArgs(t *testing.T) {
	if got := strings.Join(ColimaArgs("stop", "default"), " "); got != "stop" {
		t.Errorf("default profile args = %q", got)
	}
	if got := strings.Join(ColimaArgs("start", "work"), " "); got != "start --profile work" {
		t.Errorf("named profile args = %q", got)
	}
}
