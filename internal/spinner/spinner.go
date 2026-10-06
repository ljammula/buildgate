// Package spinner shows that a CLI command is working while it waits.
//
// On a terminal it redraws one line in place: an animated frame, a rotating
// word, the caller's status text and the elapsed time. Anywhere else (a pipe,
// a log file, a test buffer) it prints the status text as a plain line, and
// only when that text changes, so logs stay readable and output stays
// deterministic.
package spinner

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

// Frames animate the spinner; one advances every Tick.
var Frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Words rotate every WordEvery. They say the factory is busy, never what it
// is doing: the caller's status text carries the facts.
var Words = []string{
	"Working", "Forging", "Tinkering", "Assembling", "Brewing",
	"Wrangling", "Polishing", "Weaving", "Crunching", "Conjuring",
}

const (
	Tick      = 100 * time.Millisecond
	WordEvery = 6 * time.Second
)

// IsTerminal reports whether f is an interactive terminal that can take
// in-place redraws. TERM=dumb opts out.
func IsTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"
}

// Spinner is one status line. Its methods are safe for concurrent use.
type Spinner struct {
	w   io.Writer
	tty bool
	now func() time.Time

	mu      sync.Mutex
	text    string
	start   time.Time
	drawn   bool // a line is on screen (tty) and needs clearing
	printed string
	stop    chan struct{}
	done    chan struct{}
}

// New returns a stopped spinner writing to w. tty selects in-place
// animation; pass IsTerminal(os.Stdout) (or the file w writes to).
func New(w io.Writer, tty bool) *Spinner {
	return &Spinner{w: w, tty: tty, now: time.Now}
}

// Start shows text and, on a terminal, starts animating it. Calling Start on
// a running spinner only replaces the text.
func (s *Spinner) Start(text string) {
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		s.Set(text)
		return
	}
	s.start = s.now()
	s.text = text
	if !s.tty {
		s.printPlainLocked()
		s.mu.Unlock()
		return
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.drawLocked()
	stop, done := s.stop, s.done
	s.mu.Unlock()
	go s.loop(stop, done)
}

// Set replaces the status text.
func (s *Spinner) Set(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text = text
	if !s.tty {
		s.printPlainLocked()
		return
	}
	if s.stop != nil {
		s.drawLocked()
	}
}

// SetStart makes the elapsed time count from t, for a wait that began before
// the spinner did (a run's creation, a job's started_at). A zero t is ignored.
func (s *Spinner) SetStart(t time.Time) {
	if t.IsZero() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start = t
}

// Stop ends the animation and clears the line, then prints final (if
// non-empty) as a normal line.
func (s *Spinner) Stop(final string) {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop, s.done = nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLocked()
	if final != "" {
		fmt.Fprintln(s.w, final)
	}
}

// Println prints a normal line above the spinner without breaking it.
func (s *Spinner) Println(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLocked()
	fmt.Fprintln(s.w, line)
	if s.stop != nil {
		s.drawLocked()
	}
}

func (s *Spinner) loop(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.mu.Lock()
			s.drawLocked()
			s.mu.Unlock()
		}
	}
}

// Line renders the terminal line for text after elapsed.
func Line(text string, elapsed time.Duration) string {
	frame := Frames[int(elapsed/Tick)%len(Frames)]
	word := Words[int(elapsed/WordEvery)%len(Words)]
	line := fmt.Sprintf("%s %s…", frame, word)
	if text != "" {
		line += " " + text
	}
	return line + " · " + formatElapsed(elapsed)
}

func (s *Spinner) drawLocked() {
	fmt.Fprint(s.w, "\r\x1b[2K"+Line(s.text, s.now().Sub(s.start)))
	s.drawn = true
}

func (s *Spinner) clearLocked() {
	if s.drawn {
		fmt.Fprint(s.w, "\r\x1b[2K")
		s.drawn = false
	}
}

func (s *Spinner) printPlainLocked() {
	if s.text == "" || s.text == s.printed {
		return
	}
	fmt.Fprintln(s.w, s.text)
	s.printed = s.text
}

func formatElapsed(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
