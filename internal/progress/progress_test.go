package progress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendCreatesDirAndAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs", "run-1", FileName)

	if err := Append(path, Event{Source: "factory", Stage: "build", Event: "start"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := Append(path, Event{Source: "factory", Stage: "build", Event: "end", Outcome: "pass"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	events, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2: %+v", len(events), events)
	}
	if events[0].Stage != "build" || events[0].Event != "start" {
		t.Errorf("events[0] = %+v", events[0])
	}
	if events[1].Outcome != "pass" {
		t.Errorf("events[1] = %+v", events[1])
	}
}

func TestAppendStampsTsWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Append(path, Event{Source: "factory", Stage: "build", Event: "start"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	events, err := Read(path)
	if err != nil || len(events) != 1 {
		t.Fatalf("Read: %v, %+v", err, events)
	}
	if events[0].Ts == "" {
		t.Error("Ts was not stamped")
	}
}

func TestAppendPreservesGivenTs(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	want := "2026-09-17T10:00:00.123Z"
	if err := Append(path, Event{Ts: want, Source: "factory", Stage: "build", Event: "start"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	events, err := Read(path)
	if err != nil || len(events) != 1 {
		t.Fatalf("Read: %v, %+v", err, events)
	}
	if events[0].Ts != want {
		t.Errorf("Ts = %q, want %q", events[0].Ts, want)
	}
}

func TestMarkAppendsFactoryEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Mark(path, "build", "start", "", ""); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := Mark(path, "build", "end", "pass", "all good"); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	events, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2: %+v", len(events), events)
	}
	if events[0].Source != "factory" || events[0].Stage != "build" || events[0].Event != "start" {
		t.Errorf("events[0] = %+v", events[0])
	}
	if events[1].Outcome != "pass" || events[1].Detail != "all good" {
		t.Errorf("events[1] = %+v", events[1])
	}
}

func TestAppendTruncatesDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	long := strings.Repeat("x", 600)
	if err := Append(path, Event{Source: "factory", Stage: "gate", Event: "end", Detail: long}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	events, err := Read(path)
	if err != nil || len(events) != 1 {
		t.Fatalf("Read: %v, %+v", err, events)
	}
	if len(events[0].Detail) != maxDetailLen {
		t.Errorf("len(Detail) = %d, want %d", len(events[0].Detail), maxDetailLen)
	}
}

func TestReadMissingFile(t *testing.T) {
	events, err := Read(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if events != nil {
		t.Errorf("events = %v, want nil", events)
	}
}

func TestParseWorkerLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"valid round start", `FACTORY_PROGRESS {"stage":"round","event":"start","round":2,"max_rounds":6}`, true},
		{"valid round end with outcome", `FACTORY_PROGRESS {"stage":"round","event":"end","round":2,"max_rounds":6,"outcome":"fail","detail":"verify failed"}`, true},
		{"valid agent note", `FACTORY_PROGRESS {"stage":"agent","event":"note","round":2,"detail":"bash: go test ./..."}`, true},
		{"no prefix", `{"stage":"round","event":"start"}`, false},
		{"prefix without space", `FACTORY_PROGRESS{"stage":"round","event":"start"}`, false},
		{"malformed json", `FACTORY_PROGRESS {"stage":`, false},
		{"unknown stage", `FACTORY_PROGRESS {"stage":"finished","event":"start"}`, false},
		{"unknown event", `FACTORY_PROGRESS {"stage":"round","event":"bogus"}`, false},
		{"unknown outcome", `FACTORY_PROGRESS {"stage":"round","event":"end","outcome":"maybe"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, ok := ParseWorkerLine(c.line)
			if ok != c.want {
				t.Fatalf("ok = %v, want %v (ev=%+v)", ok, c.want, ev)
			}
			if ok && ev.Source != "worker" {
				t.Errorf("Source = %q, want worker", ev.Source)
			}
		})
	}
}

func TestParseWorkerLineTruncatesDetail(t *testing.T) {
	long := strings.Repeat("y", 600)
	ev, ok := ParseWorkerLine(`FACTORY_PROGRESS {"stage":"agent","event":"note","round":1,"detail":"` + long + `"}`)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if len(ev.Detail) != maxDetailLen {
		t.Errorf("len(Detail) = %d, want %d", len(ev.Detail), maxDetailLen)
	}
}

func TestParseWorkerLineNeverPanics(t *testing.T) {
	inputs := []string{
		"",
		"FACTORY_PROGRESS ",
		"FACTORY_PROGRESS null",
		"FACTORY_PROGRESS 123",
		"FACTORY_PROGRESS [1,2,3]",
		"FACTORY_PROGRESS " + strings.Repeat("{", 10000),
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ParseWorkerLine(%q) panicked: %v", in, r)
				}
			}()
			ParseWorkerLine(in)
		}()
	}
}

func TestReadFrom(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Append(path, Event{Source: "factory", Stage: "build", Event: "start"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	lines, offset, err := ReadFrom(path, 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1: %v", len(lines), lines)
	}
	if offset == 0 {
		t.Error("offset did not advance")
	}

	// Nothing new since offset.
	lines, offset2, err := ReadFrom(path, offset)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("len(lines) = %d, want 0", len(lines))
	}
	if offset2 != offset {
		t.Errorf("offset2 = %d, want unchanged %d", offset2, offset)
	}

	// A new line appended after the first read is picked up from offset.
	if err := Append(path, Event{Source: "factory", Stage: "build", Event: "end", Outcome: "pass"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	lines, _, err = ReadFrom(path, offset)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"outcome":"pass"`) {
		t.Fatalf("lines = %v", lines)
	}
}

func TestReadFromMissingFile(t *testing.T) {
	lines, offset, err := ReadFrom(filepath.Join(t.TempDir(), "nope.jsonl"), 5)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if lines != nil || offset != 5 {
		t.Errorf("lines=%v offset=%d, want nil,5", lines, offset)
	}
}

func TestPathInDir(t *testing.T) {
	got := PathInDir("/data/runs/run-1")
	want := filepath.Join("/data/runs/run-1", "progress.jsonl")
	if got != want {
		t.Errorf("PathInDir = %q, want %q", got, want)
	}
}

func TestAppendModeAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	if err := Append(path, Event{Source: "factory", Stage: "build", Event: "start"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
}
