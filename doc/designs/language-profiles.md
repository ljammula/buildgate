# Language profile contract

Language profiles are compiled, data-only hints that help an operator or
coding agent prepare a repository for a run. They do not execute commands,
choose a worker image, install dependencies, alter acceptance rules, or write
to the target repository. `.factory.yml` remains the repository's execution
contract.

## Profile fields

| Field | Meaning |
|---|---|
| `Markers` | Root-level regular files that identify a project family. Doctor detects them in the working tree and reports all matches; it does not pick a winner. |
| `RequiredBinaries` | Executable hints for a project family. Missing profile-only binaries are doctor warnings, not run prerequisites; only committed `.factory.yml` `required_binaries` are checked as run prerequisites against the final image. Doctor checks committed required binaries against the configured base image as an early diagnostic. |
| `VersionSources` | Files worth inspecting for a declared runtime version. These are hints, not claims that BuildGate parses or installs every version. |
| `ToolchainProvisioning` | `automatic` for Go, Python, Java/Maven and Ruby/Bundler derivation, `configured-image` for Node, or `image-only` for runtimes that must already be in the operator-selected image. |
| `DependencyManifests` | Common dependency and lock files for the project family. |
| `Lockfiles` | The dependency manifests a package manager writes to pin resolved versions. Together they are what the factory recognises as a dependency lockfile by name alone: the release policy's dependency-lockfile rule (`release_allow_dependency_lockfile_changes`), pinned-commit refusals, the change-size count and the plan review notes. A new ecosystem's lockfile goes here. |
| `ProjectImageManifests` | Root-level files currently consumed by `make project-sandbox-image`. This list is narrower than `DependencyManifests`. |
| `RegistryProxyEcosystems` | Ecosystems routed by the per-run package-registry proxy: Go modules, npm, PyPI, Maven Central and RubyGems. These are not tied to a required lockfile. |
| `InstallCommand`, `CachePaths`, `InstallRequires` | A reviewed candidate for a `.factory.yml` `sandbox:` block (Node: `npm ci` into `node_modules`; Python: a virtualenv install into `.venv`), and the root file that must exist for it to apply (a lockfile or `requirements.txt`). `onboard`, `init -write-factory-yml` and `quickstart` write it as a draft; the operator commits it and approves its hash (`factoryd sandbox approve`). A profile never installs anything itself. Empty `InstallCommand` means no candidate |
| `VerifyCommand`, `FullSuiteCommand` | Reviewed command candidates. Empty means the profile does not make that recommendation because a manifest alone does not identify the project's test runner. The Go profile uses `go build ./... && go test -run '^$' ./...` for verify (test compilation plus the vet subset `go test` runs, not the full `go vet`) and `go test ./...` for the full suite. `DetectVerifyCommand` is the one command `init`, `onboard`, `quickstart` and `doctor -fix` write when they write a single `verify_command` (which then also serves as the full suite): a Makefile `verify:` target, then `test:`, then the test command (`FullSuiteCommand`, else `VerifyCommand`; Node's `npm test` only when `package.json` declares the `VerifyScript`) of the one matching profile that has one, and none when several matching profiles have one. A matching profile with no command (Python; a `package.json` with no test script) neither suggests nor blocks. `full_suite_command` falls back to `verify_command` when omitted by the repository. |
| `TestPatterns` | Candidate patterns for `.factory.yml` `test_patterns`, and, together, the global default (`DefaultTestPatterns`, every profile's patterns once): a repository with no `test_patterns` gets them, whatever its language, for the tests_red_on_base gate, the plan-time feasibility check, the change-size count, the challenger's held files and the plan review notes. A production file whose name matches one (a class `SpeedTest.cs`) counts as a test there; a repository narrows the set with its own `test_patterns`. Profiles omit a pattern when one safe recommendation does not cover common layouts; Rust's `*_test.rs` is incomplete (inline `#[test]` functions and files under `tests/` are not covered), kept because the default has always held it and nothing broader is safe for every repository. |
| `BaselineFailureFormat` | The baseline parser supports `go-test` and `pytest`; `unparsed` means it cannot match failures by test name. This describes parser support, not the project's selected test runner. |

`factoryd content` lists every compiled profile, its source path, and a hash;
`factoryd content -show language-profile:<id>` prints the full metadata. Add or
change a profile in `internal/languageprofile/profile.go` and its tests. There
is no runtime profile/plugin loading or second configuration format.

## Detection and setup boundaries

Doctor's detection checks only the supplied working-tree directory's root
markers. It does not search nested modules, read project scripts to infer
commands, or resolve a monorepo's workspace layout. No match and multiple
matches produce explicit advisories; neither case selects a profile. For one match,
`factoryd doctor -target-repo <repo>` prints the marker, command candidates,
binary requirements, version sources, dependency manifests, image-helper
support, registry-proxy support, and baseline failure format. Missing
`.factory.yml` values are shown as a reviewable candidate block, never written
by `doctor`. Since `.factory.yml` is read from committed HEAD, working-tree
markers are onboarding hints and do not claim that the corresponding project
is what the next run will build.

For an automatic Go, Python, Java/Maven or Ruby/Bundler toolchain, doctor
reports the derived-image plan when the project-image planner can satisfy the
declaration, even when `FACTORYD_AUTOSTART=0`; it fails only when no project
image can be derived. Java/Gradle stays `image-only`: Gradle is not derived.

The helper currently stages only manifests at `PROJECT_DIR`'s root. Its
dependency support is Go (`go.mod`, `go.sum`), npm (`package.json`,
`package-lock.json`), pip (`requirements.txt`), Maven (`pom.xml`), and Ruby
Bundler (`Gemfile`, `Gemfile.lock`). Other manifests, nested manifests, and
toolchains not derived by BuildGate belong in an operator-built worker image.
The session config selects that image; a repository profile cannot name or
replace it.

Profile test patterns do not change the always-on `tests_red_on_base` gate. Copy
appropriate patterns into committed `.factory.yml` when the repository's test
names are not covered by the built-in defaults. Baseline named-failure parsing
is currently limited to Go and pytest; other failing baseline formats are not
matched to ticket test names and remain fail-closed. This metadata does not
weaken that behavior or any release-policy protection.
