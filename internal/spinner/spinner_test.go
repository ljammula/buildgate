package spinner

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer lets the animation goroutine and the test share a buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPlainOutputPrintsEachDistinctTextOnce(t *testing.T) {
	var buf syncBuffer
	s := New(&buf, false)
	s.Start("spec drafting")
	s.Set("spec drafting")
	s.Set("planning")
	s.Println("note")
	s.Stop("done")
	want := "spec drafting\nplanning\nnote\ndone\n"
	if got := buf.String(); got != want {
		t.Fatalf("plain output = %q, want %q", got, want)
	}
}

func TestTerminalOutputRedrawsInPlaceAndClears(t *testing.T) {
	var buf syncBuffer
	s := New(&buf, true)
	s.Start("building")
	time.Sleep(3 * Tick)
	s.Println("round 1 done")
	s.Stop("accepted")
	got := buf.String()
	if !strings.Contains(got, "\r\x1b[2K") {
		t.Fatalf("terminal output has no in-place redraw: %q", got)
	}
	if !strings.Contains(got, "building") {
		t.Fatalf("terminal output lacks the status text: %q", got)
	}
	if !strings.Contains(got, "\r\x1b[2Kround 1 done\n") {
		t.Fatalf("Println did not clear the spinner line first: %q", got)
	}
	if !strings.HasSuffix(got, "\r\x1b[2Kaccepted\n") {
		t.Fatalf("Stop did not clear the line and print the final text: %q", got)
	}
}

func TestLine(t *testing.T) {
	cases := []struct {
		text    string
		elapsed time.Duration
		want    string
	}{
		{"planning", 0, "⠋ Working… planning · 0s"},
		{"planning", 250 * time.Millisecond, "⠹ Working… planning · 0s"},
		{"", 7 * time.Second, "⠋ Forging… · 7s"},
		{"building · round 2/3", 72 * time.Second, "⠋ Tinkering… building · round 2/3 · 1m12s"},
		{"x", 2*time.Hour + 5*time.Minute + 24*time.Second, "⠋ Brewing… x · 2h05m"},
	}
	for _, c := range cases {
		if got := Line(c.text, c.elapsed); got != c.want {
			t.Errorf("Line(%q, %v) = %q, want %q", c.text, c.elapsed, got, c.want)
		}
	}
}

func TestSetStartCountsElapsedFromEarlierTime(t *testing.T) {
	var buf syncBuffer
	s := New(&buf, true)
	s.Start("planning")
	s.SetStart(time.Now().Add(-125 * time.Second))
	s.Set("planning")
	s.Stop("")
	if got := buf.String(); !strings.Contains(got, "2m05s") {
		t.Fatalf("elapsed not counted from SetStart: %q", got)
	}
}

func TestSetStartZeroIsIgnored(t *testing.T) {
	var buf syncBuffer
	s := New(&buf, true)
	s.Start("planning")
	s.SetStart(time.Time{})
	s.Set("planning")
	s.Stop("")
	if got := buf.String(); !strings.Contains(got, " · 0s") {
		t.Fatalf("zero start leaked into elapsed: %q", got)
	}
}

func TestStopWithoutStartIsSafe(t *testing.T) {
	var buf syncBuffer
	New(&buf, true).Stop("")
	if got := buf.String(); got != "" {
		t.Fatalf("Stop on an unstarted spinner wrote %q", got)
	}
}
