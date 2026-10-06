package main

// Fixture values shared by the oracle integration tests.

const ocOracleSrc = "package mood\n\nimport \"testing\"\n\nfunc TestOracle(t *testing.T) {}\n"

const ocTarget = "internal/mood/zz_oracle_test.go"

func ocManifest(extra string) string {
	return `[{"criterion":"mood is set","oracle_file":"mood_oracle_test.go","target_path":"` + ocTarget + `"` + extra + `}]`
}
