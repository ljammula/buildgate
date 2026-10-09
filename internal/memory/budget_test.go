package memory

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func applyErr(t *testing.T, err error, what string, have, limit int) {
	t.Helper()
	var ae *ApplyError
	if !errors.As(err, &ae) || !errors.Is(err, ErrApply) {
		t.Fatalf("err = %v, want an *ApplyError", err)
	}
	if ae.What != what || ae.Have != have || ae.Limit != limit {
		t.Fatalf("refusal = %+v (%v), want %s %d/%d", ae, err, what, have, limit)
	}
}

func TestApplyAddsAndRemoves(t *testing.T) {
	current := []string{"- a human line with *anything* in it", "- b.", "- c."}
	got, err := Apply(current, []string{"- d.", "- e."}, []string{"- b."}, Budget{}, 5)
	want := []string{"- a human line with *anything* in it", "- c.", "- d.", "- e."}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply = %v, %v, want %v", got, err, want)
	}
	if !reflect.DeepEqual(current, []string{"- a human line with *anything* in it", "- b.", "- c."}) {
		t.Fatalf("Apply changed its input: %v", current)
	}
	got, err = Apply(nil, nil, nil, Budget{}, 5)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty change = %v, %v", got, err)
	}
}

func TestApplyBudgetLines(t *testing.T) {
	current := []string{"- a.", "- b."}
	_, err := Apply(current, []string{"- c."}, nil, Budget{Lines: 2}, 5)
	applyErr(t, err, ApplyOverLines, 3, 2)
	if !strings.Contains(err.Error(), "3 lines") || !strings.Contains(err.Error(), "budget of 2") {
		t.Fatalf("the refusal does not name the numbers: %v", err)
	}
	// The caller makes room; Apply never picks a line to drop.
	got, err := Apply(current, []string{"- c."}, []string{"- a."}, Budget{Lines: 2}, 5)
	if err != nil || !reflect.DeepEqual(got, []string{"- b.", "- c."}) {
		t.Fatalf("with room made: %v, %v", got, err)
	}
	if _, err := Apply(current, nil, nil, Budget{Lines: 1}, 5); err != nil {
		t.Fatalf("a section already over budget with nothing added: %v", err)
	}
	if got, err := Apply(current, nil, []string{"- a."}, Budget{Lines: 0, Chars: 1}, 5); err != nil || len(got) != 1 {
		t.Fatalf("a removal alone needs no room: %v, %v", got, err)
	}
}

func TestApplyBudgetChars(t *testing.T) {
	// "- aaaa." is 7 bytes and counts 8 with its newline.
	_, err := Apply([]string{"- aaaa."}, []string{"- bbbb."}, nil, Budget{Chars: 15}, 5)
	applyErr(t, err, ApplyOverChars, 16, 15)
	if _, err := Apply([]string{"- aaaa."}, []string{"- bbbb."}, nil, Budget{Chars: 16}, 5); err != nil {
		t.Fatalf("exactly at the budget: %v", err)
	}
	var forty []string
	for i := 0; i < DefaultBudgetLines; i++ {
		forty = append(forty, "- line "+strings.Repeat("x", i+1))
	}
	_, err = Apply(forty, []string{"- one more."}, nil, Budget{}, 5)
	applyErr(t, err, ApplyOverLines, DefaultBudgetLines+1, DefaultBudgetLines)
}

func TestApplyMaxChanges(t *testing.T) {
	current := []string{"- a.", "- b.", "- c."}
	_, err := Apply(current, []string{"- 1.", "- 2.", "- 3."}, []string{"- a.", "- b.", "- c."}, Budget{}, 5)
	applyErr(t, err, ApplyOverChanges, 6, 5)
	if _, err := Apply(current, []string{"- 1.", "- 2."}, []string{"- a.", "- b.", "- c."}, Budget{}, 5); err != nil {
		t.Fatalf("five changes: %v", err)
	}
	// A line already present is not a change.
	if _, err := Apply(current, []string{"- a.", "- 1.", "- 2.", "- 3.", "- 4.", "- 5."}, nil, Budget{}, 5); err != nil {
		t.Fatalf("five additions and one already there: %v", err)
	}
	_, err = Apply(current, []string{"- 1."}, nil, Budget{}, 0)
	applyErr(t, err, ApplyOverChanges, 1, 0)
}

func TestApplyRemovingAMissingLine(t *testing.T) {
	_, err := Apply([]string{"- a."}, nil, []string{"- a"}, Budget{}, 5)
	applyErr(t, err, ApplyMissing, 0, 0)
	_, err = Apply([]string{"- a."}, []string{"- b."}, []string{"- b."}, Budget{}, 5)
	applyErr(t, err, ApplyMissing, 0, 0)
	_, err = Apply([]string{"- a."}, []string{"- a."}, []string{"- a."}, Budget{}, 5)
	applyErr(t, err, ApplyBothWays, 0, 0)
}

func TestApplyDuplicates(t *testing.T) {
	got, err := Apply([]string{"- a.", "- b.", "- a."}, []string{"- c.", "- c.", "- b."}, []string{"- a.", "- a."}, Budget{}, 2)
	if err != nil || !reflect.DeepEqual(got, []string{"- b.", "- c."}) {
		t.Fatalf("Apply = %v, %v", got, err)
	}
}

func TestApplyRefusesLinesTheFenceCouldNotRead(t *testing.T) {
	for _, l := range []string{"no dash", "- ", "- a\nb", "- " + BeginMarker, "- " + EndMarker, "- a\x00"} {
		_, err := Apply(nil, []string{l}, nil, Budget{}, 5)
		applyErr(t, err, ApplyUnreadable, 0, 0)
		if len(err.Error()) > 200 {
			t.Fatalf("error echoes too much: %v", err)
		}
	}
}

func TestUsedCountsANewlinePerLine(t *testing.T) {
	if n, c := Used([]string{"- a.", "- bb."}); n != 2 || c != 5+6 {
		t.Fatalf("Used = %d, %d", n, c)
	}
	if b := (Budget{}).OrDefault(); b.Lines != 40 || b.Chars != 3000 {
		t.Fatalf("default budget = %+v", b)
	}
}
