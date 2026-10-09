package memory

import "strings"

// NormaliseNote trims a note item and strips one leading list marker and one
// trailing full stop, which RenderLine puts back.
func NormaliseNote(item string) string {
	s := strings.TrimSpace(item)
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") {
		s = strings.TrimSpace(s[2:])
	}
	return strings.TrimSuffix(s, ".")
}

func (st *StoreState) lessonByLine(line string) *Lesson {
	for i := range st.Lessons {
		if st.Lessons[i].Line == line {
			return &st.Lessons[i]
		}
	}
	return nil
}

// sawAgain counts runID toward l once.
func (l *Lesson) sawAgain(runID, now string) {
	if contains(l.Runs, runID) {
		return
	}
	l.Seen++
	l.Runs = append(l.Runs, runID)
	if n := len(l.Runs); n > MaxLessonRuns {
		l.Runs = append([]string(nil), l.Runs[n-MaxLessonRuns:]...)
	}
	l.LastSeenAt = moveText(now)
}

// CollectFromNotes turns the "worth knowing about this repository" items one
// run's build agent wrote into candidates. A run already in CountedRuns adds
// nothing. An item the text rule refuses is counted in refused and otherwise
// ignored: it is never repaired and never stored. A line the section holds at
// HEAD is already in force and is ignored. A line the store has is seen once
// more (a dropped one stays dropped); any other becomes a candidate, unless
// the store is at MaxLessons. It does no I/O.
func CollectFromNotes(st *StoreState, runID string, items []string, sectionAtHead []string, now string) (added, refused int) {
	if st == nil || !safeComponent(runID) || contains(st.CountedRuns, runID) {
		return 0, 0
	}
	st.CountedRuns = append(st.CountedRuns, runID)
	for _, item := range items {
		line, err := RenderLine(NormaliseNote(item))
		if err != nil {
			refused++
			continue
		}
		if contains(sectionAtHead, line) {
			continue
		}
		if l := st.lessonByLine(line); l != nil {
			l.sawAgain(runID, now)
			continue
		}
		if len(st.Lessons) >= MaxLessons {
			continue
		}
		l, err := NewLesson(NormaliseNote(item), SourceAgent, now)
		if err != nil {
			refused++
			continue
		}
		l.sawAgain(runID, now)
		st.Lessons = append(st.Lessons, l)
		added++
	}
	return added, refused
}
