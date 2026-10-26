package run

// StackBase is the earlier ticket's tip record's commits sit on: for a
// replay run the commit its commits were replayed onto, else its
// StackBaseSHA, or, for a first build recorded before that field, its
// -prior-run's result. "" for a run that is not stacked, or a round recorded
// before the field, whose tip cannot be recovered without guessing.
func StackBase(dataDir string, record *Run) string {
	switch {
	case record == nil:
		return ""
	case record.Replay != nil:
		return record.Replay.OntoSHA
	case record.StackBaseSHA != "" || record.PriorRunID == "":
		return record.StackBaseSHA
	}
	prior, err := Load(dataDir, record.PriorRunID)
	if err != nil {
		return ""
	}
	return prior.ResultSHA
}
