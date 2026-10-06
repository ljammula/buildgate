package main

// Summary is an aggregate completion-state snapshot of a todo list.
type Summary struct {
	Total int
	Done  int
	Open  int
}

// Summarize computes a Summary from the given todos without mutating them.
func Summarize(todos []Todo) Summary {
	total := len(todos)
	done := 0
	for _, t := range todos {
		if t.Done {
			done++
		}
	}
	return Summary{Total: total, Done: done, Open: total - done}
}

// PercentDone returns the percentage of done items using exact
// half-up integer rounding (no floating point). It returns 0 when Total is 0.
func (s Summary) PercentDone() int {
	if s.Total == 0 {
		return 0
	}
	return (s.Done*100*2 + s.Total) / (2 * s.Total)
}
