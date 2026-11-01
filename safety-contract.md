# Factory safety contract

Status: normative Phase 0 contract (2026-08-27; condensed 2026-09-25)

This document is the reviewable safety boundary for unattended factory work.
It describes what the factory may do, what it must refuse, and how a run
reaches a terminal state. It is intentionally independent of any particular
Go package, Temporal deployment, runner, or operator console. A control marked
`partial` or `not yet implemented` is a known prerequisite for unattended
operation up to an accepted, mergeable state, not permission to bypass that
control. Merge and deploy stay outside that scope permanently (decided
2026-09-08).

This file states rules and names where they are enforced. The review history
behind each rule (dates, findings, incident write-ups) lives in git history
and `CLAIMS.md`'s gap register.

## Scope and trust boundaries

The factory control plane owns workflow truth, policy decisions, credentials,
durable evidence, and release decisions. A worker (including the coding agent,
build scripts, test commands, and model client) is untrusted and may be
incorrect, compromised, or terminated at any point.

The trust boundaries are:

| Boundary | Trusted side | Untrusted side | Required rule |
|---|---|---|---|
| Acceptance | Factory-owned canonical oracle | Agent-authored tests and reports | Only the external canonical oracle can satisfy verification. |
| Filesystem | Control-plane data and host | Selected task workspace | The worker can write only the selected workspace and its own per-launch, disk-backed scratch volume (Go build/temp and module caches, the Maven local repository, Java's temporary directory, Bundler and RubyGems gems), created for that launch and removed with its sandbox, which no other container -- canonical verify in particular -- ever mounts; Compose bind inputs are materialized read-only from the immutable base commit; evidence is written by the factory. |
| Network | The run's OpenShell network policy | Worker process | The worker has no network interface on any step; no direct host or cloud access. Its only egress is the OpenShell supervisor, which admits a connection only when the run's policy names the destination and the calling binary. With the registry proxy on, one such destination is the run's registry-proxy container, which fetches with GET or HEAD only from a fixed set of public registries (npm, PyPI and its file host, the Go module proxy and checksum database, Maven Central, RubyGems; each replaceable by the operator's mirror) and the redirect hosts each route lists; it injects no credential. |
| Credentials | Control plane / secret broker | Worker and generated subprocesses | Host SSH, cloud, GitHub, and override credentials never enter a worker. A route's model credential is stored in the OpenShell gateway's credential store on the sandbox VM; the worker holds a placeholder, and the supervisor substitutes the value only on a request to an endpoint the route's profile binds. The supervisor does not strip a credential header the worker writes itself. Immediately before each launch (`sandbox.RunThroughRuntime`) factoryd stores the current host-file value there, unless this process last stored exactly that value (compared by SHA-256, held in memory only) and the provider's revision is still the one that store returned (`openshell.Runtime.PushCredential`): a store makes every running sandbox on the route drop its model requests in flight, so an unchanged value is not stored again, while a new process, a changed value and a provider anything else has changed are. It refuses a launch whose credential expires within the step's budget (`RoutePolicy.RouteCredential`). |
| Container boundary | Host runtime | Worker container | The worker has no Docker socket and cannot control sibling containers. The OpenShell gateway holds the Docker socket; the worker cannot reach the gateway API. |
| OpenShell gateway | factoryd's `LaunchSpec` validation and host control | Worker container; Compose sidecars | The gateway is host-root equivalent inside the sandbox VM: UID 0, the Docker socket, and the operator's home directory mounted read-only. The gateway refuses a bind whose source it cannot see, and a worktree binds its repository's `.git`, so the gateway must see every repository as well as the data root (`~/buildgate`); in a colima VM set up as USAGE.md describes, the home directory holds only what the VM shares (the data root and the repositories). The mount adds nothing the Docker socket does not already give it. It listens on VM loopback only (`127.0.0.1:17670`, host-networked) and is attached to no network a sidecar or worker can join; a client needs the factory's client certificate (`~/.config/factoryd/openshell/mtls/`, directory 0700, `tls.key` 0600). It launches a worker only from a `LaunchSpec` the factory validated: OpenShell's resource admission is off (bind mounts need it off), so `LaunchSpec.Validate` and `withResolvedMounts` are the only checks on mounts (workspace and inputs resolved and contained, Git metadata read-only, no Docker socket). factoryd's host control refuses to stop or restart it while a request or sandbox is active (`hostcontrol.ActiveBuilds`); a restart by any other path is detected (SC-017). |
| Supervisor | OpenShell's pinned supervisor image; on a network that re-signs TLS, a local image built from it whose one change is its trust file | Worker process | One per sandbox, host-networked, uid 65534, read-only root. It terminates the worker's TLS, holds resolved credentials in memory, and dials upstreams from the VM's network namespace. It opens no TCP listener on the VM: only Unix sockets under `/run/openshell`. It trusts the public roots built into it and its image's `/etc/ssl/certs/ca-certificates.crt`. On a network that re-signs TLS (a corporate proxy), where `make install` recorded the proxy's CA with the system's roots in `~/.config/factoryd/build-ca.pem`, `factoryd` builds a local image from the pinned one with that bundle as the trust file (`hostcontrol.EnsureSupervisorImage`, named after the pin and the bundle) and the gateway starts supervisors from it: the supervisor then accepts the proxy's certificate for the route's one admitted upstream, so the model credential crosses that proxy, as every other tool's on that network does. The policy's host, path and executables are unchanged, and a network that does not re-sign TLS runs the pinned image. |
| Worker image | The operator's `sandbox_image` (for an API-started run, the daemon's image allow-list) and factoryd's one derivation from it | The target repository's toolchain declarations (`go.mod`, `.python-version`, `pyproject.toml`, `pom.xml`, `.java-version`, `.sdkmanrc`, `Gemfile`, `.ruby-version`) | A repository cannot name an image. Its declarations are read as version numbers only (`internal/toolchain`); a `pom.xml` or `Gemfile` also declares that Maven or Bundler is needed. When the configured image's Go, Python, Java, Maven, Ruby or Bundler does not satisfy one, the run derives an image from the configured one by a fixed rule (`toolchainImageFor`; `internal/sandbox/Dockerfile.project`, stage `toolchains`): the official `golang`, `python`, `maven:3.9-eclipse-temurin-<major>` or `ruby` image of that version, copied over the configured image's toolchain under `/usr` and `/opt`. The Ruby stage also copies libyaml and any shared library the interpreter needs from that `ruby` image, and fails when the configured image has no C compiler; nothing is installed from a package manager. The Java stage writes Maven's global settings, one mirror of every repository at `${env.BUILDGATE_MAVEN_MIRROR}`, root-owned and read-only. No file of the repository enters that build and none of its code runs in it; when Python is replaced the build runs buildgate's own script tests on the new interpreter and fails unless they pass. The derived image is pushed to the local registry and launched by digest, the digest the run records. The derivation runs inside the run, after an API caller's `sandbox_image` passed the allow-list, so an allowed image is the only base of one. Node is never replaced: the coding agents run on it and it is the executable the network policy admits. `FACTORYD_AUTOSTART=0` turns the derivation off. Residual: the toolchain is an official image named by version tag, resolved when the image is built, not a digest buildgate pins. An install image (the Install image row) is derived from the run's image the same way: by a fixed Dockerfile with no RUN step, pushed to the local registry and launched by digest. |
| Go modules | The operator's own `go` command and Go settings on the host (`GOPROXY`, `GOPRIVATE`, git's credential helper), and the registry proxy | The module paths, versions and hashes the target repository's `go.sum` files list | A build reaches a module only the operator can fetch without any credential entering a container. As a run with a registry proxy is dispatched, `factoryd` reads the `go.sum` files of the commit the build starts from (never the checkout's working tree, and not those under `vendor/` or `testdata/`) as module paths, exact versions and `h1:` hashes only (`parseGoSums`: anything else is dropped). Of those it keeps the ones the operator's Go settings mark as this machine's to fetch (`GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`; every listed module when `GOPROXY` is the operator's own), and the operator's `go` fetches exactly those on the host, in an empty directory, so it reads no file of the repository (`repositoryGoModuleDir`). A fetched module is served only when it has the hash `go.sum` gives it. The files are kept in one cache under `~/buildgate/gomodules`; a run is given a view holding its own list and no other repository's, never changed once a run has it, mounted read-only into its registry-proxy container. The proxy answers the Go route from the view first, opened as an `os.Root`, and answers 404 itself for anything else about a module the view holds, so a private module's name is not sent to the public proxy (`registryproxy.Route.LocalDir`), and the worker's `GONOSUMDB` names each module the view holds, so none is looked up in the checksum database. The worker's `go` is made to use the proxy for every module (`GONOPROXY=none`) and verifies what it is served against `go.sum` as always; a module `go.sum` lacks is verified against `sum.golang.org`, read through the proxy's checksum-database route (`GOSUMDB=sum.golang.org <route URL>`) and checked with that database's built-in key. The view's path is in the Workflow input, so a Worker that takes over the run serves the same modules. Residuals: a `go.sum` on the build's base commit that names, with its real hash, a private module of another repository the operator can read gets that module served to the build (the list is the committed file's, never the agent's edit of it during a build); the host's `go` contacts whatever host a listed private module path resolves to, as the operator's own `go mod download` would. `FACTORYD_AUTOSTART=0` turns the fetch off. |
| Spend meter | `factoryd-meter` (`internal/meter`), called by the supervisor for every model request | Request and response bodies of the worker's model calls | Counts each run's tokens and cost and refuses requests past the ceiling. It parses untrusted bodies, holds no credential, writes each sandbox's ledger under `~/buildgate/meter-ledgers/` where no worker and no other run's sandbox writes, fails closed (`on_error: fail_closed`), denies WebSocket upgrades, and refuses a response it cannot read as a stream. A request whose usage it cannot read is charged its estimate; the one exception is a 200 event stream that ends in `response.failed` with no usage after only `response.created` and `response.in_progress` events, which is released uncharged, and one that carries usage on the failure is charged that usage. The meter reads event types only, never an error message or code. Exposure as it is: a plaintext gRPC listener published on VM loopback only (`127.0.0.1:17672`), with no TLS and no caller check; no worker or sidecar can reach it, and any process on the VM or the Mac's loopback can. |
| Compose sidecars | The operator's `compose_services_*` settings and `internal/composeservices`' field allow-list | The target repo's compose file and `.env` at the run's base commit | Repo content can only select allow-listed images, Linux platforms (`linux/amd64` or `linux/arm64`), and fields; fixed `container_name` values are inert and never emitted; relative bind inputs are read from the immutable base commit, reject links/special files/path escapes, and are mounted read-only (a source the base commit lacks becomes an empty per-run named volume); it never widens a sidecar's containment (a service's own `mem_limit` is accepted only at or below the operator's `compose_services_memory`). Operator-supplied worker endpoint variables are host/session configuration, sorted and collision-checked, cannot override factory/sandbox/credential/cache/network variables, and never enter Temporal history; a value that names a service as a host is refused, since the worker resolves no service name. They are stored in the gateway's sandbox record and readable through the gateway API by any holder of the factory certificate. Sidecars stay on the factory-owned internal network; the worker does not join it and reaches each sidecar by address and port through the supervisor. A compose file the allow-list rejects halts the whole run before its build (`compose_services_rejected`, naming each service and the fix), never a partial or silent launch. Published `ports:` are never published on the host: each fixed TCP mapping becomes a forward on the worker's own loopback to that service's address on the run's internal Compose network, and two services publishing one port reject the file. A compose edit made by the worker is never launched in the same run. |
| Repository commands | The committed `.factory.yml` as read before the workflow starts | The worktree copy; the build | `verify_command`, `gates:`, `setup:`, `autofix:` and `sandbox.install` run only in the sandbox (`autofix:` only in the build sandbox, inside a round, with its changes confined to files the ticket already changed), from values resolved before the workflow starts, with whatever network that step already has (none for verify and gates beyond the registry proxy and Compose sidecars; the model route in the build and challenger sandboxes); `setup:` and `gates:` are given to no planner or reviewer (a setup command that fails inside a build round is named, with its output, to that build's own agent), and nothing from the file runs in a review sandbox; `.factory/` is mounted read-only into every sandbox that runs a repository command, as the commit `.factory.yml` was read from holds it, and a run that changes it is refused at release. A `sandbox:` block's `sandbox.install` runs only after its block's SHA-256 is approved (`factoryd sandbox approve`), and a run or a submit with an unapproved block proceeds no further: the approval guards what an install produces and what is cached and reused across runs, and does not decide which commands may run in the sandbox, since `setup:` and `verify_command` run there unapproved |
| Install image | The operator's approval of the `sandbox:` block's SHA-256 (`factoryd sandbox approve`) and factoryd's fixed install-image build (`internal/sandbox/install_image.go`, `Dockerfile.install`) | The committed `sandbox:` block; everything its install commands write; a build's edits to the worktree | The install that produces a reused image runs only in an install sandbox (the run's image and limits, the registry proxy narrowed to the block's registries, no model route, no Compose services) on a scratch worktree of the commit `.factory.yml` was read from, never on the run's worktree; when the run's base commit holds other dependency inputs, the image is built from that commit with a run-local scope no other run computes, and removed when the run ends. The worker checks the approval again and that the commands it was given hash to the approved block, and each cache path must be ignored by git before the install runs; the host's git runs with no system or user configuration, global excludes, hooks, replace refs or LFS smudge. The capture reads the scratch worktree through `os.Root` and carries only regular files, directories and relative links that, resolved through the tree's other links (at most 40 followed in all, a missing name with no `..` after it), stay inside their cache path, owned 0:0 with mode 0755 or 0644, within 4 GiB and 500,000 entries. The image adds that tree with no RUN step under `--network none`, needs GNU coreutils in the image, is built once per key on the machine, is reused only when its key label matches every `InstallImageKey` input and the local registry holds it, and is launched by digest. Each build, verify and gate sandbox (never a review's) refuses a cache path with a linked parent, removes every cache path from `/workspace` and copies the image's tree before its command, then reruns the install when a key input changed. Residual: the local registry keeps the manifests of evicted images, and a failed restore is reported as a setup failure of that step. |
| Release | Factory release policy | Agent prose and reviewer verdicts | Merge/deploy requires the complete evidence package and policy pass. |
| Control-plane API | `factoryd serve`'s daemon configuration (resource ceilings, release policy, sandbox image policy) | An authenticated `POST /runs` caller | A bearer token proves authentication, not trust. A caller-supplied field that selects what code runs inside the boundary is never accepted as given: `sandbox_image` is allow-listed by operator configuration (`-api-allowed-sandbox-images`), as `sandbox_docker` already is. No caller-supplied field can select host execution. |
| Workstation queue | Docker containment (always applied) | `factoryd submit`/`worker` requests (each ticket build runs from an in-memory entry) | No path has host execution: every ticket build resolves to a configured, digest-pinned `-sandbox-image` (built locally or pulled from a tagged Buildgate release; no built-in default) or is refused before any container launches. This holds for every entry point (bare `factoryd <run>`, `serve`, the API, Temporal), not just `worker`. |
| Forge / PR review | Factory release policy and an operator-configured reviewer trust list | Branch pushes, draft-PR open, the `gh pr ready` flip, `gh pr edit --base` retargets, and reviewer comments | Each push, draft-PR open, and ready flip requires `release.Decision.Allowed`, re-checked immediately before the side effect — including a `factoryd retry` of a PR-open-only failure, which re-runs the decision against the run's frozen policy, verifies `refs/heads/<branch>` still equals the accepted `ResultSHA`, and pushes an explicit `<ResultSHA>:refs/heads/<branch>` refspec (the normal open path pins the same way). A reviewer comment triggers a corrective build only when its author is on the explicit `-pr-trusted-authors` allow-list; an ignore-list is not sufficient. An advice round (`review_advice_rounds`, SC-022) pushes to an open PR's branch while its request is `building`: it pushes only an accepted round whose `release.Decision.Allowed` (not invalidated) is re-checked immediately before, that built on that branch, and whose `ResultSHA` descends from the remote head read just before; it turns a ready PR back to draft first, pushes `<ResultSHA>:refs/heads/<branch>` (never forced) and reads the remote back, and only a read-back equal to the `ResultSHA` makes the round's run the ticket's run and PR head (`Round.Pushed`) and posts one PR comment (`gh pr comment`; a failed comment is logged). A check that refuses, a failed draft conversion included, pushes nothing and leaves the run the round followed as the PR head; for every round not pushed the local branch is moved back to that run's `ResultSHA` by a compare-and-swap that moves only a branch at a descendant of it (`workspace.RestoreBranch`), so no later round builds on or pushes the rejected commits, and a move that fails halts the request only while the branch still points past that commit. A push or read-back that fails once sent halts the request with the round unconfirmed (`pushRoundResult`, shared with a PR-review round's push). A multi-ticket request's ticket N (N>1) draft PR may open stacked on ticket N-1's own still-open branch (`-pr-base`) rather than the default branch; `advancePRReadyOrApproved` refuses the ready flip while its base is another ticket's branch, and once ticket N-1 merges, `gh pr edit --base` retargets ticket N's PR onto ticket N-1's own base only after the ready flip's checks (a fresh `release.Decision.Allowed` and a PR head equal to that run's `ResultSHA`); a failed or refused retarget leaves the PR draft, is retried on the next poll, and never halts the request. The commit each build round ends in is kept under `refs/buildgate/rounds/<request>/t<ticket>/r<n>` in the target repository's local `.git`, written by the host; those refs are created and never moved (a name already taken moves the round to the next free number, so a re-plan after a send-back keeps the earlier plan's commits), are deleted with the request's unpushed `buildgate/<request>/t<n>-from-r<m>` branches once it is `done`, `closed` or `cancelled`, are never in a push refspec, and `factoryd retry -from <n>` builds on a new local branch at one and is refused for a ticket whose branch has been pushed, so a round is never reset by a force-push. Restack. A later ticket's PR needs a restack exactly when the current tip of the branch its PR stacks on (the earlier ticket's branch on origin, `Request.StackBase`) is not an ancestor of the later ticket's PR head, or, after that ticket merged, when the commit the later ticket's commits sit on (`run.StackBase` of its PR-head run) is not an ancestor of the tip of the branch the earlier ticket merged into: a squash or rebase merge leaves it out, a merge commit does not, and that branch moving on later is no reason to restack again (`checkRestackNeed`, `internal/requestdriver/restack.go`); a fact that cannot be read decides nothing, and holds the retarget and the ready flip, as does a PR on the earlier ticket's branch whose PR-head run records no stack base; such a hold is recorded on the ticket (`Ticket.RestackHold`), named in the request's next action and in `factoryd watch`, and reminded at the reminder interval until a poll can tell. Ticket N+1 is restacked only after every earlier ticket's own restack, if it needs one, is pushed and confirmed, and not while one is parked or halted (the request is then). The replayed range is `<U>..<H>`: `U` the earlier ticket's tip recorded on the later ticket's current PR-head run (`run.StackBase`), `H` that run's `ResultSHA`; origin's branch must be at `H`, the range must hold no merge commit, and every commit in it must lie in the range a run of the ticket judged (from its diff base, or a replay run's new base, to its `ResultSHA`; the run that opened the PR, or a round or restack of it whose push was confirmed), else the restack is refused before any replay and the request parks in `resume_review` with the reason, as for a human commit on the later ticket's own branch (a human commit on the earlier ticket's branch is only a new base). The replay runs in a scratch worktree on a local-only branch `buildgate/<request>/t<n>-restack<k>` (deleted with the request's unpushed branches), under the shared Git metadata lock, as `git cherry-pick --allow-empty --keep-redundant-commits` of the listed commits (each applied, an already-applied one included; a commit that becomes empty is kept, so the pairs stay one for one, and is refused below; nothing squashed; no other ref moved), with rerere off, no signing, no system or user configuration, and the repository configuration's program-running and read-changing settings overridden (`HardenedGitArgs`, `HardenedGitEnv`). The scratch worktree's checkout and removal, the cherry-pick, and every git read of the replayed and original commits by the host and by the replay run (their ranges, patch ids, and the replay run's worktree state, `runner.GitRevListRange`, `runner.GitPatchID`, `runner.GitReplayIsClean`) also read attributes from the empty tree (`--attr-source=<empty tree>`, `workspace.ReplayGitArgs`), never from a `.gitattributes` in the commits or the worktree, so none of those selects a filter, merge or diff driver, a binary diff, or a line-ending or encoding conversion, and they set `core.autocrlf=false`, `core.eol=lf`, `core.safecrlf=false` and `merge.renormalize=false`; with `core.attributesFile=/dev/null` and `GIT_ATTR_NOSYSTEM`, the one attributes file still read is the repository's own `info/attributes`, the operator's, as its `.git/config` is (`HardenedGitArgs`). The replay run's own worktree is checked out as any run's is. The replay is refused unless it produces exactly as many commits, in order. Each pair's patch id is `git diff-tree -p -U0 --binary --full-index --no-renames` of the commit against its first parent piped to `git patch-id --verbatim` (no context lines, so a change beside one made under it keeps its id; every whitespace change and binary byte counted; `runner.GitPatchID`, the one computation the host and the replay run share), recorded on the run record beside its original's (`run.Replay`); a pair whose ids differ, an original that changes something replayed as a commit that changes nothing among them, parks the request with the reason before any replay run; the replayed result gets one round ref of its own at the next free `t<ticket>/r<n>`, from which the replayed commits are reachable, the earlier refs left unchanged. The replayed result is judged by a new run of the ticket, a replay run: it builds nothing and has no handoff and no `-earlier-attempt`; its record names the request ticket and its stack base is the new base. Its build step (`ApplyReplayActivity`) reads the replay from the run's own record, which the workflow input must match, and passes only what it re-derives on the host from git and run records: the run is of the replay's request ticket; its worktree is clean at the replayed head, with the new base under it; every run replayed from is an accepted, sandboxed run of the same repository and of that request ticket, with an allowed, not invalidated release decision and no override; the originals end at the PR-head run's `ResultSHA` and start after its stack base; the pairs are exactly `git rev-list` of `U..H` and of the new base to the replayed head, in order, with no merge commit, and each pair's patch ids, computed there (`runner.GitPatchID`), are equal (both empty only for commits that change nothing; an original that changes something with an empty replay is refused) and as recorded; and each original is in a range a run replayed from judged. Any miss halts the run with `replay_refused`; every gate and review then runs again on the replayed head, measured from the new base. A ticket has at most one restack or review round in flight (the request's steps run one at a time; an in-flight restack is recorded on the ticket, `Ticket.Restack`, and reconciled by the next pass, which, while its replay run has not ended, waits for it with the ticket's poll ended, so no second replay or round of the ticket starts, and then settles that run, never starting another for it), and a round started after a restack builds on and pushes to the ticket's PR branch at the restacked head. Before the push the factory checks that the new run's `ResultSHA` is the replayed head or descends from it (the evidence step may commit what verification left behind; `replayResultRefusal`, else the request halts), reads the remote head (it must equal the `ResultSHA` of the ticket's `Ticket.PRHeadRunID`), converts a ready PR to draft, re-checks `release.Decision.Allowed` for the new run, and pushes `<new ResultSHA>:refs/heads/<branch>` with `--force-with-lease=refs/heads/<branch>:<old>`, `<old>` that read; it reads the remote back whatever the push returned, and the new run becomes the PR-head run (`Round.Pushed`), and the local branch is moved to it, only when the read-back equals its `ResultSHA` (a push that reported an error after landing included); a read-back at the old head halts the request with nothing pushed, and one that fails halts it with the round unconfirmed. Wherever the factory finds a PR branch on origin at a commit other than the `ResultSHA` of the ticket's `Ticket.PRHeadRunID` (on every PR poll, from the PR head GitHub reports, whether or not a restack is needed; before a restack, before its push, after it, and when reconciling one in flight, where a run record or origin branch that cannot be read keeps the restack in flight and decides nothing), it first matches that commit against the `ResultSHA` of an accepted replay run of the same request ticket whose push is not confirmed (the restack in flight, or a restack round after the PR-head run's round): a match is that restack's push having landed, recorded as confirmed (`confirmLandedRestack`), and only any other commit is a human commit. A lease that fails pushes nothing and is handled as a human commit on that branch. Before every push the factory makes to an open PR's branch, a PR-review round's (`pushAcceptedRoundAndReply`) and an advice round's (`concludeAdviceRound`) as well as a restack's, a PR GitHub reports ready for review is converted to draft (`draftBeforeFactoryPush`; a failed conversion pushes nothing and halts the request), once the push is confirmed the PR is read again and one marked ready meanwhile is converted back to draft (`redraftAfterFactoryPush`; a failed read or conversion there is logged, not retried), and only the ready flip's own checks make it ready again on the new head; an approval counts only when its commit is the PR's current head. So no approval or ready state carries across a head the factory changed. A replay that conflicts parks the request in `resume_review` with the reason, no unresolved cause: `factoryd resume` replays again, which succeeds only once the branch under the ticket has moved so the commits apply, or cancel, and a fix pushed by hand to the ticket's branch is refused as a human commit; a replay run that is not accepted, or whose decision is not allowed, is quarantined like any run, pushes nothing and halts the request; a replay run that never starts is tried again on the next poll, and the third in a row since the request entered `pr_review` halts it; a host git older than `workspace.GitFloor` (2.41, for `--attr-source`) parks the request before any replay; the factory never rebuilds the ticket from its spec to resolve either. After an earlier ticket merges, a later ticket that needs a restack is neither retargeted nor flipped ready until its restack is pushed and confirmed, and a base GitHub changed on its own is not a retarget the factory made (the factory records its own as `Ticket.Retarget`). An earlier ticket closed unmerged, or whose branch is gone unmerged, parks the request. A restack is the only forced update the factory makes to a pushed branch: it never removes a round's commit from the PR branch except by replacing it with its replay, and never resets the branch to an earlier round. Ticket N+1's build, retry or resume chains from ticket N's `Ticket.PRHeadRunID` (its last confirmed-pushed round or restack run, else its first build); when ticket N's PR head is not that run's `ResultSHA`, ticket N+1 is not built until ticket N's restack or the operator resolves it, and a resume of a kept build, or a retry that would continue on an attempt's branch, whose stack base is not ticket N's current PR head is refused or rebuilt from that head. |
| CI checks | The pull request's base branch's required checks (its branch protection, as the forge reports it) (GitHub's per-check `isRequired`) and the conclusion GitHub records for each check run or commit status on the PR head | The code a check runs (the build's), every line of a check's log, the CI definition files, and every check that is not required | Only a required check that completed with a failure on the PR head the factory pushed starts a CI round; a pending check starts nothing and is named as the reason the request waits; an optional check's failure is a note; when the required set cannot be read, every completed failure counts, at most one round per check. A check's log reaches a build only as SC-018 values. The CI definition paths are built-in protected paths. See the paragraph on CI checks after SC-018. |
| Challenger | The factory's held copy of each challenge file (copied out once, SHA-256 pinned, mounted read-only), the gate sandbox, and the factory's run of it (a reproduction runner's report, else the repository's own `verify_command`) | The challenger job and every file it writes; the build whose result it challenges | The challenger reads the whole result commit in a scratch worktree that is discarded afterwards, never the run's worktree or branch; that it tests the change's public surface is an instruction in its prompt, not a containment. It is given no build session, handoff, notes or review finding. Its tests count only by the factory's own run of them, twice when red; they refuse nothing and start at most one round per ticket (SC-022). |
| Console loopback writes | `factoryd serve`'s bind address and the operator's own local processes | Any page loaded in the operator's browser, and any caller that reaches `serve` through a reverse proxy | On loopback with no override token, request-write routes accept same-origin JSON without a token from a request no proxy passed on; `POST /runs/{id}/override` and start-class routes always need a token. See [Console loopback writes](#console-loopback-writes) below. |
| Gate token | The operator's `<config name>.gate-token` file (written by `factoryd gate-token`, mode 0600, beside the session config, one per profile, with an expiry) | A console that is not on the machine, and everything on the network path to it | The token opens the reads and the request writes and nothing else; while its file exists, a request that is not local needs it to read. See [Gate token](#gate-token) below. |
| Console-originated request creation | The operator's `workspaces:` session-config list and the factory's data directory | `POST /requests` | Takes the request-write gate, never `authorizeStart`; `workspace` is never accepted as an arbitrary host path. See [Console-originated request creation](#console-originated-request-creation) below. |
| MCP endpoint | The operator's `<config name>.mcp-token` file (created by `factoryd mcp`, mode 0600, beside the session config, one per profile) | `POST /mcp` and the model driving the MCP client | Off until the token file exists; every call needs the token, with no loopback relaxation. Its tools are reads and `POST /requests` only: no tool approves, rejects, retries, resumes, cancels, edits or overrides. See [MCP endpoint](#mcp-endpoint) below. |
| Untrusted worker output | `internal/sanitize` | Agent-authored log/report text reaching triage, halt reasons, notifications, `status`, or the console | Text is stripped (ANSI/OSC, control and Unicode format characters, invalid UTF-8) and secret-redacted (`sanitize.Text`/`sanitize.Line`; Python mirror `build_app.redact`/`single_line`) before display, and shown as a quoted log excerpt, never as a factory verdict or claimed provenance — the log-writing process shares the worker's container and uid, so the factory cannot attest who wrote a given line. |
| Model-listing credentials | Factory route-validation code, reused | `doctor -list-models`, `doctorCheckCopilotModelListed`, the Copilot token-exchange client | These host-side calls send only what a launch's route would admit: validated via `sandbox.RoutePolicy.ValidateUpstreamScheme` + `sandbox.CredentialSafeForUpstream` (the same check `RouteSpec.Validate` applies), redirects refused (`meter.NoRedirectCheckRedirect`), no credential over plaintext and none when `routes.<name>.allow_no_credential` is set. |
| Model routes | Session-config `routes:` and the host-side resolver in the process launching that route's worker | Request model choice; Temporal Workflow input | A route's credential is resolved host-side only for that route's own launch; Workflow input names a route, never a credential source, and the Worker refuses any policy that differs from its own route and role model in any field except a tightened ceiling, then resolves that route's credential itself. The meter policy a sandbox is created with is derived from that same checked policy (`RoutePolicy.MeterConfig`). The route's network policy admits the harnesses' model executables (`harness.ModelBinaries`, the union across harnesses), not one harness's. |
| Skills | The operator's `skill_dirs:` folders and `roles.<role>.skills` names | Request input; the target repo's own project skills; the worker between rounds | Only a host-side, hashed, read-only snapshot of the role's named skills reaches the worker (SC-016); a same-name repo skill refuses the launch; harness homes stay per-session and get fresh copies each round. Skill content is operator-trusted instruction text, vetted like a dependency. A review launch sees the repository's instruction files as the base commit holds them, read-only, and what the build changed in them only as quoted data (SC-019). |
| Repository memory | The operator's `memory.repositories` switch (off for every repository not listed) and the project's kill switch; the operator's approval at spec review; the section text the host renders | The build agent's notes; the memory ticket's build; a reviewer's comment on its pull request | A note becomes a candidate line only if it passes a fixed text rule (`memory.ValidateReason`: one line, 120 characters, a fixed character set with no Markdown emphasis, link or list syntax, commands only inside a backtick pair that stands alone, under the command rule, no URL with a scheme (with or without its slashes; a host name written without a scheme is not recognised, and the operator reads every line before approving it), address, absolute path inside or outside a command, `..` path component, home path, unbroken token of 20 or more characters of `A-Za-z0-9+/_-` that holds both a letter and a digit, or anything redaction would change); a refused note is never repaired or stored. A candidate is collected only from a run recorded against this repository's root. A section line holding a control or format character is not read, and every line is printed with escapes removed. Candidates are shown to the operator only (`factoryd memory list`/`show`, `GET /projects/{project}/memory`). The section reaches the repository only as a request the operator approves and merges (SC-020). No command a note names is ever run by the factory, and nothing is collected, proposed or dropped without a person running the command. A memory request already in flight is stopped at release by the kill switch and by `factoryd memory off`; removing the repository from `memory.repositories` stops new proposals only. |
| Operator-cited memory evidence | The operator's `factoryd memory add -run <id>` argument | A referenced run record | The host accepts only a confirmed terminal run recorded against the selected repository root; it stores and displays the run ID separately from agent-note provenance and does not send it or its contents to a model. |

```text
  +----------+  mTLS   +----------+  Docker  +----------+
  | factoryd |-------->| OpenShell|--------->| worker   |
  | (host)   | sandbox | gateway  |  socket  | no       |
  |          | request | (VM      |          | network, |
  |          | from a  | loopback)|          | no       |
  |          | checked |          |          | socket,  |
  |          | spec    |          |          | no creds |
  +----------+         +----------+          +----------+
     |                      |                     |
     | pushes the route's   | credential     only egress
     | credential before    | store               v
     | each launch          |               +------------+   policy:    +-------+
     +--------------------->+-------------->| supervisor |--destination>| model |
                                            | (per       |  + binary    | API   |
                                            | sandbox)   |  + path      |       |
                                            +------------+              +-------+
                                                  |  every model request
                                                  v
                                            +------------+
                                            | meter      |
                                            | ceilings,  |
                                            | ledger     |
                                            +------------+
```

The selected workspace is the only mutable job input. The factory must reject
an unvalidated workspace, repository, ticket, or release target before a
worker starts. A human may inspect or override a quarantined run, but no human
response is required for the default path to continue.

### Console loopback writes

Threat scope: this defends against a *browser*: a hostile page or a
DNS-rebinding attacker reaching the loopback port through XHR, fetch, or a
`<form>` POST. It assumes a single-OS-user host. Another process running as
the same user can already forge these headers, or run `factoryd approve`/
`cancel` directly against the data directory.

Request writes (approve, reject, spec/ticket edits, read-back
answers, retry, resume, cancel, create;
`internal/api.Server.authorizeRequestWrite`):

- Without a token, a request is accepted only when all of these hold:
  - The request is local (`Server.local`): the server is bound to loopback,
    `Host` names this server's own loopback address and port
    (`127.0.0.1`/`localhost`/`[::1]`, `hostMatchesLoopback`), and no reverse
    proxy passed the request on (`forwarded`: it carries none of
    `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`,
    `X-Real-Ip`, `Via`, or a `Tailscale-*` header). The `Host` check is the
    DNS-rebinding defense. The proxy check is there because a proxy such as
    `tailscale serve` keeps the caller's `Host` header: a caller on the
    proxy's network can send the loopback address and a matching `Origin`
    itself, and the headers the proxy adds are what it cannot remove.
  - `Origin` exactly matches that origin, or, when `Origin` is absent,
    `Sec-Fetch-Site` is exactly `same-origin`. A request with neither header
    is refused. This is the CSRF defense.
  - `Content-Type` is `application/json`, which blocks a `<form>` POST's
    encodings.
- The relaxation is disabled on a non-loopback bind or whenever an override
  token is configured. `serve` never opens write routes on a wider bind
  without a token.
- `POST /runs/{id}/override` (`authorizeOverride`) always requires the bearer
  token. It moves a quarantined run to accepted/halted and records a release
  `Decision`, which is stronger than the drafting-pipeline writes above.
- On loopback, the `Host` check applies to every route, reads included
  (`Server.ServeHTTP`). `-allowed-host <host[:port]>` (`hostAllowed`) adds
  extra `Host` values for reads only, e.g. through an ssh tunnel, reverse
  proxy, or `tailscale serve`. The write relaxation still consults only
  `Server.local`, so a write through an allowed host needs a token: the
  override token or the [gate token](#gate-token).
  `POST /mcp` is reachable through an allowed host and can submit a request
  there, behind its own token ([MCP endpoint](#mcp-endpoint)).
- A request a proxy passed on must name an `-allowed-host`: one whose `Host`
  is the loopback address is refused on every route.
- An allowed host is an `-allowed-host` value, or the one host in
  `<config name>.remote-console` beside the session config, which
  `factoryd remote-console` writes and `serve` reads on each request whose
  `Host` is not otherwise accepted (`WithAllowedHostSource`). The file gets
  the token files' checks (not a symlink, a regular file, this user's, mode
  0600) and must hold exactly its four well-formed lines, the host a
  lower-case DNS name with an optional port that is not a loopback name;
  otherwise it allows nothing. Either kind of allowed host is never local.
  The command writes a gate token before it adds the listener, never uses
  a Funnel port, and removes on `-off` only what it set up.
- **Limit:** a forwarder that adds none of those headers (a TCP-level
  forward, an ssh tunnel, a proxy set to rewrite `Host` to the loopback
  address and add nothing) makes its callers local by this test. An ssh
  tunnel needs a shell on the host, which is already the operator. Behind
  any other such forwarder the gate token guards nothing (every caller is
  local), so `serve` needs an override token, which turns the write
  relaxation off, and a read token for the reads. colima's
  `host.docker.internal` is such a forwarder: every container on the VM
  reaches `serve` as a local caller.
- `serve` takes the override and read tokens from
  `FACTORYD_API_OVERRIDE_TOKEN` and `FACTORYD_API_READ_TOKEN`, else from
  `serve-override-token` and `serve-read-token` beside the session config
  (`tokensFromFiles`), which `factoryd install-service -require-tokens`
  creates (mode 0600, exclusive create, never inside a git work tree) and
  never writes into the plist. Such a file that exists and fails the token
  files' checks stops `serve` from starting: the protection is never
  dropped silently. `install-service -allowed-host` puts `-allowed-host`
  into the serve plist's arguments.
- `-require-tokens` also creates `serve-console-token` the same way, which
  `serve` reads with them: the credential of the console on this machine,
  which cannot hold the read token ([Console token](#console-token)).

A rejection or a send-back over HTTP (`POST /requests/{id}/reject`) names
the stage it was made on: the request's `state` and `entered_at` as the
caller read them (`request.Seen`). It is applied only while the request is
still in that stage, checked under the request lock, and refused with 409
otherwise, so a decision written for one draft cannot land on the stage or
the redraft that followed. It binds the stage, not the text: an edit made
in place while the request stays in that stage does not change `entered_at`.
A comment on a passage (`anchors[].quote`, `factoryd reject -comments-file`)
is bound to the text: under the same lock, its file must be `spec.md` (at
`spec_review`) or a current ticket file (at `plan_review`), its section a
required heading present in that file, its item no larger than the spec's
criteria count, and its quote a byte-for-byte substring of its section, else
the rejection is refused with 409 (`request.ErrPassageMoved`; a place the
document cannot have is 400, `request.ErrBadAnchor`) and nothing changes. A redraft that follows such comments is scoped by buildgate, not by
the model: the model writes the whole document, and the driver copies every
section nobody commented on from the reviewed file byte for byte
(`request.SpliceSections`), hand edits included, and refuses a redraft that
does not bound a commented section as the skeleton does (a missing or renamed
next heading, an extra heading of its level) or that has a skeleton heading
inside a code fence; a scoped plan redraft revises the rejection's snapshot of
the tickets, and a send-back to the spec ends the scope; the result goes through the
same structural checks and, once approved, the same hashes as any draft. A
whole-file comment on `spec.md`, a free note beside the comments, or a
send-back makes a full redraft.
An approval is bound to text instead, by the hashes of the files it
approves: a spec or plan, when the caller sends them.

Start-class routes (`POST /runs`, daemon lifecycle, and the `GET` release
and stats routes; `authorizeStart`) get no relaxation. A missing or wrong
bearer token is always a 403. The token is handled as follows:

- **Per-process token.** When `FACTORYD_API_START_TOKEN` is unset and no
  stable token file exists, `serveMain` generates a 32-byte `crypto/rand`
  token that lasts for the life of the process. An operator-set env var
  always wins unchanged.
- **Delivery.** The token is delivered only as a URL *fragment* in the
  printed console link (`http://<addr>/#t=<token>`,
  `consoleLinkWithStartToken`). It is never a query parameter, because
  browsers never transmit fragments (RFC 3986 §3.5).
  `captureStartTokenFromLocation` (`console/src/platform/startToken.ts`) stores
  it in origin-scoped `localStorage`, then strips it from the address bar
  with `history.replaceState`. The token exists in only three places:
  `serve`'s own stdout, that fragment, and that browser's storage.
- **Stable token for a supervised `serve`.** `factoryd install-service`
  creates `<session config dir>/serve-start-token` once, so a restart does
  not invalidate stored tokens (`cmd/factoryd/serve_start_token_file.go`;
  the file checks are `cmd/factoryd/token_file.go`).
  - The file is mode `0600`. It is never written into the LaunchAgent plist,
    and never passed through a child's env or argv: `quickstart`'s `serve`
    child re-derives the path from `-config` and reads the file itself.
  - `serve`, `factoryd console`, and `install-service` all resolve the path
    through the one shared `resolveEffectiveConfigPath`/
    `serveStableStartTokenPathFor` pair.
  - `factoryd console` only reads the file, never creates it.
    `uninstall-service` leaves it in place.
- **Hardening of the stable token:**
  - The token is appended to a printed or opened link only when
    `consoleBaseIsOwnLoopbackServe` confirms the link points at this
    process's own loopback `serve`. A remote `FACTORYD_CONSOLE_URL` gets a
    link without the token. So does a loopback address whose listener
    `quickstartServeVerifiedOurs` cannot attribute to this data dir
    (`TestQuickstartPrintAndOpenConsoleLinkOmitsTokenForAnUnverifiedListener`).
  - `quickstart` reuses an already-listening `serve` only after
    `quickstartServeVerifiedOurs` attributes the listener to this session
    (`lsof` plus a same-uid pid check). A bare `/healthz` 200 is not enough.
    The pids it checks are this data dir's own: its `quickstart-serve.pid`,
    the pid in the `console-address` record its `serve` wrote (for the
    address that record names), and the `serve` LaunchAgent's when the
    service is installed for this data dir
    (`TestServeVerifiedOursIgnoresAnotherDataDirsLaunchAgent`).
  - A listener it cannot attribute (another data dir's `serve` on the
    default port) never gets the token: this data dir's `serve` is started
    on a free loopback port, and the token is returned only once that
    `serve` has recorded itself
    (`TestQuickstartEnsureServeTakesAFreePortWhenTheDefaultIsHeld`,
    `TestQuickstartEnsureServeGivesNoTokenUntilItsOwnServeIsRecorded`).
  - `openInBrowser` never puts a token-bearing URL on argv. It opens a
    `0600` redirect file in a fresh `0700` temp dir, then removes it.
  - When the token comes from the stable file, `serve`'s startup log prints
    the link *without* the fragment, because a standing credential does not
    belong in a log.
  - `inspectTokenFile` `Lstat`s the file and trusts it only if it
    is a regular file owned by the current uid with `mode&0o077 == 0`. The
    file is created with `O_CREATE|O_EXCL|O_NOFOLLOW`, and creation is
    refused inside a git work tree (`insideGitWorkTree`).

### Console-originated request creation

- **Gate.** `POST /requests` (`internal/api.Server.createRequest`) only
  creates a request in `submitted`. It cannot move a run past a safety gate,
  so it takes `authorizeRequestWrite`, not `authorizeStart`.
- **Workspace allow-list.** `workspaceAllowed` requires the canonicalized
  path to meet both conditions:
  - It is a git repository root (`internal/requestsubmit.GitToplevel`).
  - It is either on the operator's `workspaces:` list or already the
    `Workspace` of an existing request in this data directory.
- **Shared validation.** `internal/requestsubmit.Submit` is the one
  validation path shared with `factoryd submit`, and its guards still apply
  to an allow-listed workspace:
  - the workspace is a directory;
  - `-data-dir` is not inside it;
  - no project-basename collision;
  - a resolvable verify command.
- **`GET /workspaces`.** This is a read route (`authorizeRead`). It returns
  the allow-list plus each workspace's verify-command hint. That is nothing
  `GET /requests` and the caller's own filesystem access don't already
  reveal.

### Gate token

The credential of a console that is not on the machine (`internal/api`'s
`WithGateToken`, `cmd/factoryd`'s `gate_token_cmd.go`).

| Property | Rule |
|---|---|
| What it opens | The read routes (`authorizeRead`) and the request writes (`authorizeRequestWrite`): create, approve, reject, retry, resume, cancel, the spec and ticket edits, and the read-back answers. A request created with it may carry every field `POST /requests` accepts, a verify command included: it is the operator's credential, and the same holder approves the plan |
| What it never opens | `POST /runs/{id}/override` (`authorizeOverride` compares against the override token only), the start-class routes (`authorizeStart`), and `POST /mcp` (`serveMCP` compares against the MCP token only). The MCP token and the start token open no gate route |
| Who needs it | While the gate is on, a request that is not local (`Server.local`, above), or any request once a read token is set, reads only with the gate token or the read token (`gateRequired`), and `/console-config.json` asks the console for the gate token. The console's own files, `/console-config.json` and `/healthz` stay open: the page has to load before it can be given the token |
| Local use | The gate decides nothing for a local request: it reads and writes as [Console loopback writes](#console-loopback-writes) says |
| The file | `<config name>.gate-token` beside the session config: the token and `expires <RFC 3339 time>`. Written beside and renamed into place, never through a symlink, never inside a git work tree. `serve` reads it on each request that needs it, with the start-token file's checks (not a symlink, a regular file, this user's, mode 0600) |
| States | No file: the gate is off. A usable file: on. A file `serve` cannot use (it fails a check, is malformed, has expired, or holds `disabled`): **on with no gate token**, so no gate token opens anything and a request that is not local reads only with the read token and writes only with the override token, when those are set. A file that goes bad never reopens the reads it guarded |
| One value, one class | A gate token equal to the override, start, read or MCP token is not usable |
| Lifetime | 12 hours unless `-ttl` says otherwise (one minute to 30 days). `-rotate` replaces it: the old token is refused on the next request, and a stream opened with it ends at its next poll |
| Who is recorded | A write the gate token alone authorized is recorded under the name the console sent (`api` when it sent none) with ` (gate token)` added by the server (`writePrincipal`). A request write or a run override whose name already carries that text is refused, so only the server writes it. The name is the caller's claim; the suffix is what the server knows. |
| In the browser | The console takes the token from a password field on its gate screen, or from `#gate=<token>`, which it removes from the address bar; either way it is stored only once the server has accepted it, in `sessionStorage`, and sent only as an `Authorization` header. The field is a masked text field, not a password field, so the browser does not offer to save it. **Residuals:** a `#gate=` link can remain in the browser's history (a pasted token is never in a URL); script running in the console's page could read `sessionStorage` (the console renders model-written text as text, never as HTML). The token's expiry, `-rotate`, and its having no override power bound both |

### Console token

The credential of the console on the machine once `serve` requires a token
from every caller (`internal/api`'s `WithConsoleToken`, `cmd/factoryd`'s
`serve_token_files.go` and `consoleTokenFragment`).

| Property | Rule |
|---|---|
| What it opens | What the gate token opens, the reads and the request writes (`gateBearer`, `consoleBearer`), and only for a local request (`Server.local`): a copy of it opens nothing through a proxy |
| What it never opens | `POST /runs/{id}/override`, the start-class routes and `POST /mcp`, as for the gate token |
| The file | `serve-console-token` beside the session config, written by `install-service -require-tokens` with the other token files (mode 0600, exclusive create, never inside a git work tree, kept across a reinstall, never in the plist). `serve` reads it at start; a file that fails the token files' checks stops `serve` from starting. It has no environment variable and does not expire |
| One value, one class | A console token equal to the override, start, read or MCP token opens nothing, and a gate token equal to it is not usable |
| Who is recorded | A write it authorized is the operator at this machine: recorded under the name the console sent, with no suffix, as a local write without a token is |
| In the browser | `factoryd console` and `quickstart` append `&c=<token>` to the start-token link they open, only for this data dir's own loopback `serve` (`consoleLinkTokens`). The console removes it from the address bar and stores it only once `/console-config.json` answers `accepted`, in `localStorage`, like the start token, so every tab keeps it. A gate token held by the tab is used first; a stored console token the server refuses is forgotten. **Residuals:** the `#t=…&c=…` link can remain in the browser's history; script in the console's page could read `localStorage`. Its holder must also be on the machine, and it has no override power. A colima container is local too (above): like the override and read token files, this one is out of a container's reach only while the session config is outside every directory the container VM mounts |

### Notifications

What tells the operator a request waits on them (`internal/requestdriver`'s
`prepareRequestNotification`, `internal/notify`, the console's
`useBrowserNotifier`).

| Property | Rule |
|---|---|
| What one can do | Open the request's own page. No notification, on any channel, approves, rejects or otherwise passes a gate: a gate is passed on the page that shows the text, under [Console loopback writes](#console-loopback-writes) or the [gate token](#gate-token) |
| What a link carries | The address of a console and `/requests/<id>` or `/runs/<id>`: never the start, gate, override, read or MCP token, in a path, query or fragment. The address is the one the data dir's own `serve` recorded, or `FACTORYD_CONSOLE_URL` (for `submit`'s printed link, `-console-base-url`), each used only when it is an http or https address with no user information, query or fragment and treated as unset otherwise (`consolelink.BaseURL`); a path prefix the operator sets there is kept as given. A link handed to another program (`open`, a Slack or Discord webhook) is that and nothing else; a browser notification carries no link at all, only text |
| The link for another machine | `https://<host>` as `factoryd remote-console` recorded it in the data dir (`consolelink.RemoteBaseURL`), used only while the `<config name>.remote-console` record it names exists, and only when it parses as an https URL of a host with no user, path, query or fragment. The page it opens asks for the gate token; the link does not supply it |
| One notification for one event | A run that is a request's ticket sends nothing to the operator itself (`dispatchRunNotification`); its halt, quarantine or acceptance is on its own record and `notifications.log`, and the request's notification follows from the same outcome: `HaltRequest`, `quarantineRequestWithCheck`, `EnterResumeReview`, or the PR-review poll (`notifyPullRequestWaiting`: ready to merge, marked ready and not ready to merge, or still not ready to merge ten minutes after the head was first checked; a pull request stacked on one that has not merged waits for that one's notification). A quarantine the factory follows with a corrective round of its own notifies nobody: it does not wait for a person. A run with no request, and a run halted through `POST /runs/{id}/override`, send their own (SC-013). **Residual:** a pull request whose review state cannot be read from GitHub is never checked, so it notifies nobody; the worker logs each failed read |
| The host's banner standing down | The desktop banner for a request is not shown while a console tab on the host raises the notification itself. The tab says so with `GET /requests/events?notifier=1`, and `serve` records it (a file in the data dir, fresh for 20 seconds after the stream's last poll) only for a request that passes the loopback write's local and same-origin checks (`loopbackSameOriginWrite`): a page of another origin, a caller with neither `Origin` nor `Sec-Fetch-Site`, and a caller through a proxy record nothing. The log line, Slack and Discord are never held back. **Residual:** a tab whose browser shows no notifications (Focus, the site muted in system settings) still holds the banner back; the log line and the console's own "Needs you" column remain |
| Text shown | The headline is the factory's own. The request's title and a halt or quarantine reason pass through `internal/sanitize` as on every other operator surface, and the console renders them as text |

### MCP endpoint

Threat scope: the caller is a model, steered by whatever it reads (a chat
message, a spec, a diff). The endpoint gives it the means to start work and
follow it, and no means to pass a gate.

- **Protocol.** JSON-RPC, version negotiation, the Streamable HTTP
  transport (stateless, JSON responses) and argument validation against
  each tool's schema are the official Go SDK's
  (`github.com/modelcontextprotocol/go-sdk`). `serveMCP` checks the token
  before the SDK sees a request. The SDK's own localhost `Host` check is
  off, because it would refuse every tunnelled request; `Server.ServeHTTP`'s
  check, which knows `-allowed-host`, has already run.
- **Off by default.** `POST /mcp` (`internal/api.Server.serveMCP`) answers
  404 until the operator's `<config name>.mcp-token` file exists
  (`config.mcp-token` for `config.yml`). The config's name is in the file
  name, so turning the endpoint on for one profile's `serve` leaves
  another's off. `serve` reads the file on every call (`mcpTokenSource`),
  so `factoryd mcp -rotate` and `-disable` take effect at once; a rotation
  that cannot write the new token leaves the old one working. A file that
  is a symlink, not owned by the operator or readable beyond its owner
  counts as absent. `factoryd mcp` prints the token only beside the
  address this data directory's own `serve` recorded.
- **Token on every call.** The bearer token is compared in constant time.
  `loopbackSameOriginWrite` is never consulted, and the start, read and
  override tokens do not open the endpoint.
- **Fixed tool table.** `mcpTools` is the whole surface:

  | Tool | Route replayed |
  |---|---|
  | `list_requests` | `GET /requests` |
  | `get_request` | `GET /requests/{id}` |
  | `get_run` | `GET /runs/{id}` |
  | `get_run_diff` | `GET /runs/{id}/diff` |
  | `list_workspaces` | `GET /workspaces` |
  | `submit_request` | `POST /requests` |

- **One route per tool.** Each tool is pinned to the mux pattern of its
  route, and `mcpReplay` refuses a call the mux would hand to any other.
  An id is caller text: `events` as a request id would otherwise reach
  `GET /requests/events`, a stream that never ends.
- **No gate decision.** A tool call is replayed through the server's own
  mux carrying an in-process mark (`mcpCaller`). Only `authorizeRead` and
  `createRequest` honour the mark; `authorizeRequestWrite`,
  `authorizeOverride` and `authorizeStart` do not, so the mark opens no
  approve, reject, retry, resume, cancel, editor, override, start or
  release route even if a tool were added that named one.
- **Submit keeps its guards.** `submit_request` passes through
  `workspaceAllowed` and `requestsubmit.Submit` unchanged, so a model can
  submit only against a workspace the operator listed, and the session's
  token, cost and budget ceilings bound what a submission can spend.
- **Submission limit.** At most 5 `submit_request` calls an hour create a
  request (`mcpSubmitAllowed`), counted per `serve` process: text a model
  reads can tell it to submit again and again, and each request has its
  own ceiling. The session's monthly budgets are the durable bound.
- **Console links.** Each request a request tool returns
  (`list_requests`, `get_request`, `submit_request`) carries `console_url`,
  its console page, built from the `Host` of the `POST /mcp` call, and only
  when that `Host` is one `serve` accepts (`hostAllowed`: its loopback
  address or an `-allowed-host`); on a wider `-addr`, which checks no
  `Host`, any other gets no link (`mcpWithConsoleURLs`).
  The scheme is https over TLS or behind a proxy unless the proxy's
  `X-Forwarded-Proto` says otherwise. The link carries no token: from
  another machine the console still asks for the gate token, and the MCP
  token opens no console route.
- **Reads.** The MCP token grants the reads above even when a read token
  is configured. Spec, ticket and diff text in a result is model-written;
  results are capped at 256 KiB (a request list keeps its newest rows).
- **Reach.** The endpoint is a route of `serve`, so the bind address and
  the `Host` check apply: from another machine it needs `-allowed-host`
  (a tunnel or `tailscale serve`) or a wider `-addr`. `serve` speaks plain
  HTTP; the tunnel carries the token.

## Threat model

| ID | Threat / failure mode | Preventive control | Required response | Current status |
|---|---|---|---|---|
| T-01 | Agent changes or substitutes the acceptance oracle | Keep the oracle outside the worker's writable boundary and hash the exact oracle used | Halt and quarantine on oracle drift or inaccessible oracle | partial |
| T-02 | Worker reads or exfiltrates host/cloud credentials | Do not inject credentials; the route's model credential is held in the OpenShell gateway's store and substituted by the supervisor outside the worker. Residual: a credential header the worker writes itself is forwarded | Halt, quarantine, and asynchronously notify; never wait for approval | partial |
| T-03 | Worker writes outside its selected workspace | Explicit writable-root allow-list and containment/escape checks | Terminate the complete process tree, then halt and quarantine | partial; a reproduced linked-worktree `.git` pointer escape was closed 2026-09-04 (`containment-matrix.md` Filesystem row) |
| T-04 | Worker reaches arbitrary network services | Network-denied baseline; the run's OpenShell network policy names every admitted destination and calling binary | Terminate the worker and quarantine on an escape or policy violation | partial |
| T-05 | Agent claims success without implementing the required change | Factory-owned oracle, diff, scope, and evidence checks | Treat prose as evidence only; fail the policy check | enforced in bounded slices |
| T-06 | A timeout or crash loses the real outcome | Durable attempt, event, artifact, and state records; recovery marks unknown work ambiguous | Halt and quarantine rather than rerun an ambiguous side effect | partial |
| T-07 | Two workers mutate one workspace concurrently | One worker lease / repository owner at a time; isolated runs and `-on-branch` runs share the repository (an `-on-branch` run also holds its branch's lock exclusively), a challenge round or other shared-checkout run holds it alone | Queue the request or halt on an ownership violation | partial; covered for build execution (shared/exclusive repository flock, `factoryd-git.lock` for shared Git metadata / `RepositoryOwnerWorkflow`), the kill switch, and `factoryd intake` (`-pilot-dir` lock, 2026-09-16) |
| T-08 | A failure silently waits for a person | Explicit halt → quarantine → asynchronous notification path | Persist the failure and return; never block on a human | partial |
| T-09 | An override is unauditable or can bypass authentication | Require authenticated operator identity and reason; append durable history | Reject the override and retain the prior state | partial |
| T-10 | Merge/deploy occurs before safety evidence exists | Release policy checks the complete evidence package and phase prerequisites | Keep the run quarantined; no release side effect | partial; every accepted run gets a fail-closed durable decision (plus the `factoryd kill-switch`), which gates the forge/PR surface. No merge or deploy side effect exists, by design (SC-001) |
| T-11 | Cancellation leaves a child process or container running | Cancel the complete process/container tree and record cancellation: the sandbox is deleted through the gateway, with `docker rm -f` of its containers as the fallback when the gateway is unreachable | Halt; quarantine if outcome is unknown | partial |
| T-12 | Workspace history is rewritten during a run | Capture base revision at execution time and require it to remain an ancestor | Halt before evaluating gates or committing further work | enforced in bounded slices |
| T-13 | Target-controlled Compose content escapes the sidecar boundary or misroutes the worker | Parse only allow-listed fields; allow only Linux platforms; ignore fixed container names; materialize relative binds from the base commit as read-only regular files/directories; attach sidecars only to the factory-owned internal network, which the worker reaches by address through the supervisor and never joins; validate and inject operator worker endpoints without protected-key overrides | Reject the entire Compose configuration before build or worker launch and quarantine | enforced in bounded slices |
| T-14 | The sandbox runtime restarts mid-run and starts a worker's command a second time | Host control refuses to stop or restart the gateway while a request or sandbox is active; a per-launch guard lets a worker command run once, and a second start exits before touching the worktree (SC-017) | The step is recorded lost; a lost build keeps its worktree and waits in `resume_review` | partial |
| T-15 | The gateway, a supervisor or the meter is compromised | OpenShell images pinned by digest; mTLS for gateway clients; sandbox JWT; `factoryd doctor` checks the pinned images, the gateway's TLS and mTLS settings, and both services' health | The operator stops the gateway (`factoryd stop -all`) | partial |
| T-16 | A build, or the repository it builds, plants text in failing output, a file name or a reviewer's finding, or its own notes, to steer the next attempt or its reviewer | The record of an earlier attempt is built by the factory from the run record; every value from a build or a reviewer is one line, length-capped, cleaned and quoted as data; it goes only to a build's first prompt, never into a spec or a review (SC-018); the hash and state are checked before use | The corrective build is a new run whose result every gate and an independent review judge against the unchanged spec; its record, the handoff it was given and the prompts its launches saved are kept in the run and request directories (a saved prompt is what the session folder held when the host copied it: a build can alter its own, and the host empties the folder before every launch so an earlier step's file is never kept as a later launch's) | partial |
| T-17 | A build edits, adds or links the repository's instruction files (AGENTS.md-style files, harness folders, project skills, MCP or hook configuration) so the reviewer that reads its worktree is told to pass it | Every model review launches with the base commit's instruction files mounted read-only over the worktree; what the build changed in them reaches the reviewer only as fenced data; instruction-named paths the worktree holds beyond the result commit are removed first; a shape the mask cannot carry halts the run before the review launches (SC-019) | The run halts with `review_instructions_failed`; the cause is in the review attempt's record for the operator and in no handoff, spec or later build | partial; the table of instruction paths is a list, and a harness may load a path that is not on it; the table was measured against the pinned pi, codex and copilot harnesses, and against pifork with the pinned pi behind its launcher (what an operator's own fork adds is not measured), by a recorded probe (`make probe-instruction-paths`, result in `agent/pi/tests/fixtures/instruction_paths.json`), which found copilot loading `.github/copilot-instructions.md` and `.github/instructions` from the directory of a file it read, so every table entry matches at any depth; `.github/mcp.json`, which copilot runs only in a trusted project, is on the table although a worker turn never grants that trust |
| T-18 | A build edits or replaces a script under `.factory/` that a repository command runs, so a gate judges the build with the build's own script | Every build, verify and gate sandbox mounts `.factory/` read-only from the commit `.factory.yml` was read from; a worktree shape the mount cannot carry halts the run (`factory_dir_failed`) before the sandbox starts | The snapshot's SHA-256 is on each attempt; `.factory/` is a protected path, so a result commit that changes it is refused at release | partial; a script that calls files outside `.factory/` runs the build's copy of them, as `verify_command: make test` does |
| T-19 | A build plants an instruction in its notes so that it becomes a line of `AGENTS.md` every later agent obeys | The text rule refuses markup, quotes of shell syntax, URLs and anything over one short line; the host renders the line and the whole file; one request changes at most five lines; the operator reads the exact section in the spec at spec review; the release check passes only a byte-equal `AGENTS.md` of mode 100644 under that one name and no other changed file, only while the base still holds the `AGENTS.md` the text was rendered from, and refuses any other run that changes the file of a repository with a memory section or adds the marker text to one without; the memory request's own reviews read the base commit's instructions (SC-019); a reviewer's comment starts no build that rewords a line (SC-020) | A line in force is removed by a memory request (`factoryd memory propose -remove`), or by an ordinary human commit; `factoryd memory off` and the kill switch stop further proposals and the release of a memory request already in flight; the `memory.repositories` switch stops further proposals only | partial; a plausible but wrong line can be approved by a person, and no command a line names is checked |
| T-20 | A secret, a hostname or a personal path reaches a public repository's `AGENTS.md` through a memory line | The text rule refuses URLs in any form (`//`, `www.`), address-shaped tokens, home paths, absolute paths (outside a quoted command a `/` followed by a letter or `.` unless a letter, digit, `.`, `_` or `/` precedes it; inside one a `/` followed by a letter, `.` or `/` at its start or after a space or `=`), e-mail addresses, any unbroken run of 20 or more characters of `A-Za-z0-9+/_-` that holds at least one letter and at least one digit (key ids, hashes, encoded secrets; a long word, relative path or variable name with no digit passes) and any text secret redaction would change; a repository's candidates, stop marker and proposals are kept under a key of its name and root path, never shared with a repository of the same name, and candidates are collected only from runs recorded against that root; memory is off for a repository until the operator lists it; the operator reads every line at spec review and again on the pull request | The line is removed by a memory request or a human commit; history rewriting is the operator's own action | partial; the rule is pattern-based: a bare internal name, a secret of letters only or digits only, and a path after a character other than a space or `=` inside a command pass it |
| T-21 | A challenger spends a round on a test of behaviour the spec never asked for or a broken test; a build steers the challenger (through instruction files or text in its diff) into tests that pass vacuously; a challenge file attacks the host or is edited by the build it judges | Each challenge file names, in a structural header, the numbered acceptance criterion or read-back answer it derives from; the factory holds its copy on the host, checks its SHA-256 before each mount, and runs it only in a gate sandbox with no model route on a scratch worktree, by a reproduction runner's per-test report where one covers the file, else by the repository's own `verify_command`'s exit status, and a red result only when two launches agree; the challenger launches with the base commit's instruction files (SC-019) and none of the build's session, handoff or notes; a red challenge starts at most one round and never quarantines (SC-022) | A declined or still-red challenge is listed in the pull request with the build's one-line reason; the person who merges reads it | partial; whether a test really tests the criterion its header cites is not checked; a wrong test costs one round; a vacuous test passes and is committed into the pull request |
| T-22 | A build edits the CI that judges its pull request, or a check's log plants text to steer the build of a CI round | The CI definition paths are built-in protected paths; a check's log reaches a build only as SC-018 values (one line each, cut, cleaned, quoted as data); only a required check's completed failure starts a round, at most one per check per ticket | A run that changes a CI definition path is denied release; the same required check failing again parks the request in `resume_review` | partial; a file a CI workflow calls (a `Makefile`, a package script) is not protected, as for `.factory/` (T-18); the CI definition paths are a closed list |
| T-23 | A build shapes its change to read smaller than it is (code in files that match the test patterns, files it marks generated) so an oversized change opens one pull request, or places code of an earlier `pr_order` group under a later group's paths so a mixed change opens one pull request | The size is measured on the host from git numbers only; `test_patterns` and the generated and lock-file lists are read from the commit `.factory.yml` was read from, never the result commit, and git reads no attributes file of the run's commits | The change opens one pull request at its measured size; the person reviewing reads the diff | partial; a file that matches `test_patterns` and holds production code counts as a test; `pr_order` groups are path patterns, so a migration written outside its group's paths ships with the code |

## Safety invariants

These are contract identifiers, not suggestions. A change that weakens one
requires a new contract review and an updated machine-checkable test.

- **SC-001 — fail closed before release:** no unattended merge or deploy is
  ever allowed — permanently, by standing operator decision, not a
  precondition still to demonstrate.
- **SC-002 — no host credentials:** workers and their subprocesses receive no
  host SSH, cloud, GitHub, or factory override credentials.
- **SC-003 — no Docker control:** workers receive no Docker socket or equivalent
  host-container control interface.
- **SC-004 — selected workspace only:** worker writes are confined to the
  validated task workspace; control-plane data and the canonical oracle are
  outside that writable root.
- **SC-005 — network deny by default:** worker network access is denied unless
  an explicit, narrow proxy policy grants a particular route.
- **SC-006 — external oracle only:** agent-authored tests, prose, and reviewer
  verdicts are never sufficient to advance a state; canonical verification and
  factory-owned policy checks decide pass/fail. A reviewer's finding never
  refuses a run. A challenger's test (SC-022) never refuses a run either: the
  factory's run of it can start one round and is otherwise advice, and a
  challenge that passes moves no run toward accepted.
- **SC-007 — durable attributable override:** every human override records the
  operator, reason, prior state, new state, and time; an override is never a
  required approval step.
- **SC-008 — durable reconstruction:** state, attempts, events, policy results,
  and artifact hashes are durable so a crash can be reconstructed; an unknown
  side effect is ambiguous, not an automatic success or safe-to-rerun result.
- **SC-009 — bounded attempts:** every attempt has time, token, retry, and cost
  limits, and retry classification distinguishes infrastructure, verification,
  oracle-drift, and implementation failures.
- **SC-010 — complete cancellation:** cancellation terminates the complete
  subprocess/container tree and does not leave an untracked child running.
- **SC-011 — exclusive ownership:** at most one worker owns a slice; each
  isolated execution mutates only its own worktree and branch; an `-on-branch`
  run holds its branch's lock exclusively and the repository shared, and
  reconciles stranded worktrees only under an exclusive hold it took while the
  repository was idle; an execution that mutates a shared checkout, or writes
  the repository-wide `info/exclude` (a challenge round), holds its repository
  exclusively, and an exclusive reconcile removes the run blocks a crashed run
  left in `info/exclude`; factory
  mutations of shared Git metadata (worktree, branch, `info/exclude`) are
  serialized.
- **SC-012 — no oracle mutation:** the job cannot modify its canonical
  acceptance oracle; oracle and verify-surface hashes are recorded with the
  attempt; the verify surface includes the committed `.factory.yml` the run
  read (its SHA-256 on the run record), its `setup:` commands, which run
  before the command of every build, challenger, verify and gate sandbox and
  never in a review sandbox, and the `.factory/` directory of the commit that file was
  read from, mounted read-only in each of those sandboxes (its snapshot
  SHA-256 on each attempt). A failed gate's rerun on the base commit is one
  of those gate sandboxes: the same launch path, setup commands and
  `.factory/` mount, on a scratch worktree, never on the host. A failed
  verify, full-suite or baseline command's flaky-test rerun (SC-017) is one
  too, on the run's own worktree. A challenger's test file (SC-022) is held
  on the host: copied out of the challenger's output once, SHA-256'd on the
  challenge record, checked against that hash before each mount, and mounted
  read-only into each sandbox that runs it or that an advice round's build
  reads it in; no build sees a writable copy.
- **SC-013 — asynchronous quarantine:** every failed or ambiguous policy check
  transitions to halt/quarantine and emits an out-of-band notification without
  waiting for a human response.
- **SC-014 — release evidence boundary:** an `allowed` release decision is
  reachable only after policy checks validate oracle results, diff
  scope/size, protected paths, and dependency changes; confirm that a
  rollback-plan reference and base/result commit SHAs are recorded; and
  account for override history. The decision gates the forge/PR surface:
  a denied run's branch is never pushed into a draft PR, and an open PR is
  never marked ready once its decision is denied or invalidated, nor left
  ready across a change of head the factory made: before every push the
  factory makes to an open PR's branch, a PR-review round's, an advice
  round's or a restack's, a ready PR is turned back to draft, once that push is
  confirmed a PR someone marked ready meanwhile is turned back to draft
  again (a read or conversion that fails there is logged, not retried),
  and only the ready flip's checks make it ready again (the Forge row). No code
  acts on it toward merge or deploy — those remain human actions,
  permanently. See [Release-policy defaults](#release-policy-defaults).
- **SC-015 — harness and model are closed presets:** a request's harness
  resolves only to a compiled-in preset, and its model only to an
  operator-configured `models:` entry within its role's operator-configured
  `allowed` list; that model reaches an upstream only through an
  operator-configured route (`routes:`), chosen in the model's declared
  order by host-side pre-launch checks, never
  by the request. No request, model, alias, or role input can name an
  executable, interpreter, upstream URL, path prefix, route, or credential
  source, and every preset runs inside the same unconditional Docker sandbox.
- **SC-016 — skills are operator snapshots:** a worker sees only the skills
  its job's role names in `roles.<role>.skills`, resolved through the
  operator's `skill_dirs:`, snapshotted and SHA-256'd host-side by the
  launch itself and mounted read-only at `/inputs/skills`; that digest is
  recorded on the attempt (or a drafting job's evidence). On the Temporal
  path the Activity mounts Workflow-input skills only when they equal its
  own Worker's resolution for that role, and computes the digest itself. No request input selects or names a
  skill, no host harness home (`~/.codex`, `~/.copilot`, `~/.claude`,
  `~/.pi`) is ever mounted, and a target-repo project skill sharing a
  configured skill's folder or front-matter name refuses the launch.
- **SC-017 — a lost Activity is never rerun on its own:** on the Temporal
  path every Activity of a new `RunWorkflow` execution runs at most once
  (`MaximumAttempts: 1`, `activity-retries` version 2). A step lost because
  its worker stopped (a heartbeat or start-to-close timeout) puts its
  request in `resume_review`, and nothing reruns until a human chooses
  `factoryd resume` (continue the lost build in its kept worktree from the
  last completed round, or rerun a lost drafting or planning step),
  `factoryd resume -from scratch` (rebuild the ticket) or `factoryd cancel`.
  A lost build keeps its worktree, never its harness session, and every
  gate reruns on the final commit. A heartbeat RPC that fails on the
  connection to the Temporal server does not cancel a step: a long step
  runs under `workflow.GuardStep`, which ends it only on the server's own
  answer (a cancel request, a pause or reset, an Activity it no longer
  knows), on its worker stopping, or when no heartbeat of its own has
  succeeded for the Activity's whole heartbeat timeout. The last is the
  cause `temporal_unreachable`: the run halts with it and its worktree
  kept (found from the isolation marker when the halt cannot be confirmed
  against the server), never as an operator's cancellation. Every Temporal
  worker factoryd builds sets the SDK's heartbeat throttle to 15 s
  (`workflow.HeartbeatThrottle`), so the SDK gives up on a stalled heartbeat
  after about 30 s and the guard heartbeats on its own every 10 s, counting
  its silence from the last heartbeat that could have got through: a stall of
  up to about 90 s loses nothing, and a longer one parks the request once as
  `temporal_unreachable`, or as `worker_lost` when the server timed the step
  out first. A step the server timed out while a step of the same process is
  still running its run (or another process holds a fresh owner heartbeat) is not parked by `HaltLostRequestStep`: no second build starts
  beside it, and when its run later halts as `temporal_unreachable` the
  request's cause becomes that. A resume runs only after that decision,
  only when the gateway, which must be reachable, reports no sandbox under
  the name the run recorded before launch, and no worker or supervisor
  container carrying that `openshell.ai/sandbox-name` is alive, and only when the worktree is on the run's own branch with HEAD equal to,
  or a descendant of, the HEAD its round state recorded; a decision answers
  one Generation of `resume_review`, and a decision for an earlier
  Generation is ignored. A workflow whose history recorded `activity-retries`
  version 1 still replays with its retry of the build, verify, full-suite,
  repository-gate and review Activities, at most once, only
  after an infrastructure failure or a Temporal timeout. Such an attempt
  after the first runs only after it takes the Activity execution's lease,
  its request's
  `-sandbox-docker` and `-data-dir` match the Worker's own, and every
  container labelled with the run and data dir is confirmed removed; the
  lease is re-checked before every sandbox launch, and a lower attempt's
  completed-checkpoint save is refused once a higher attempt holds it. Its
  meter ceilings are the configured ceilings minus the spend the run's
  meter ledger recorded since the execution's first attempt began
  (the whole ledger when that start is unknown; an unreadable ledger
  refuses). A retried build keeps the earlier attempt's worktree but never
  its harness session. Post-build and collect-evidence are never retried.
  The same holds for any step that ended with no verdict on the work, a
  closed set of causes each read from a fact the factory observed and never
  from a worker's or a provider's wording: `worker_lost` (the cases above,
  and a build whose command the sandbox runtime started twice),
  `temporal_unreachable` (factoryd heard nothing from the Temporal server for
  a whole heartbeat timeout while the step ran) and
  `sandbox_launch` (no launch of the step released its command: the route
  credential could not be stored, the sandbox could not be created, or it
  had no worker container; and the sandbox's removal was confirmed). A
  command that was released and then timed out, printed past the output
  limit or lost its wait ran, and is not one, also when a later launch of
  the same step never started; nor is a launch the Activity itself gave up
  on, or one whose removal could not be confirmed, which keeps nothing. A
  verify, gate or review command started twice is an infrastructure
  failure: one sign of it is an exit code the repository's own command
  could return.
  `dependency_fetch` is the third cause: a canonical verify, baseline
  verify, full-suite or repository-gate command exited non-zero in a launch
  during which the run's registry proxy counted at least one request its
  upstream failed, was run once more in the same Activity on the worktree
  as that run left it, with no model call, and exited non-zero again with
  at least one more such request counted. A second run that passes is the
  step's result, and one that fails with none counted is an ordinary
  failure. The proxy counts a request whose registry could not be reached,
  did not answer within the upstream timeout, broke off its response, or
  answered 5xx; it counts no 4xx, no request it refused (method, path, a
  redirect outside the route's allowlist, its own concurrency limit), none
  its client abandoned, and none served from its cache or a local
  directory. A worker can provoke a counted failure (a client that reads a
  response more slowly than the upstream timeout), so this cause too only
  hands a failed step to a human. The count is read from the proxy container's log before and
  after each launch: with no registry proxy, or when either read fails,
  the step is judged as if nothing had been counted. The second run is not
  made when less than twice the first run's duration is left of the
  Activity's time, and one the deadline leaves no room to start changes
  nothing. Limits: the count shows that a registry request failed during
  the launch, not that the command failed because of it; a package manager
  that gives up on a request before the proxy's upstream timeout is not
  counted; a gate's rerun on the base commit and the factory's run of a
  challenge are judged without it. A baseline that
  ends so halts with this cause, never `baseline_verify_failed`.
  `sandbox_disk` and `sandbox_memory` are the next two, read from Docker
  and never from a command's output. Before anything is begun for a
  launch (its registry proxy and compose services, whose images are pulled
  at the first attempt, on both launch paths) and again before its command
  is released, the factory reads the free space of Docker's data
  filesystem (the `statfs` of `/` inside the always-running meter container,
  whose overlay root is on it; `factoryd-meter statfs`) and refuses the
  launch with `sandbox_disk` below 2 GiB free, for every step, the build and
  the model jobs included: nothing ran, and the refusal ends the launch
  loop. It reads the space again after the command exits and before the
  sandbox is deleted, and once more when everything the launch started is
  gone: its sandbox and scratch volume, its compose services and its
  registry proxy (a reading is not taken when the runtime could not remove
  the scratch volume or the compose project's volumes could not be
  confirmed gone, and not between the attempts of one step, which share a
  proxy). A canonical verify, full-suite or repository-gate command that
  exited non-zero with under 1 GiB free after its exit, and whose launch
  released, in all that, less than half of what was free before it (so the
  launch did not fill the disk itself), is `sandbox_disk` with no second
  run; a baseline verify needs only the low free space. The limit of that
  reading: it counts what the launch's own sandbox, scratch volume, compose
  services and proxy held, not the worktree, which is the host's, and not
  anything else another process wrote to Docker's disk meanwhile, so a
  worker that fills the disk through the worktree (native Linux, where it
  is on Docker's filesystem) can read as not having filled it itself. The
  rule only ever hands a failed step to a human.
  `sandbox_memory` is the baseline verify's alone: a baseline verify that
  exited non-zero in a sandbox whose worker container Docker reports
  `State.OOMKilled` (the daemon sets it on any out-of-memory kill in the
  container's cgroup, not only of its first process, and only a restart
  clears it) ends the Activity unresolved, since it ran on the base commit
  before any work was built. For any later step (verify, full suite,
  repository gate, setup) an OOM-killed failure is a judged failure, because a build
  can cause its own OOM: the command is not run again, the step fails as it
  would without the reading, and the attempt records the fact. The handoff
  to the next build then carries one factory-written sentence for that check
  ("the kernel killed a process of this step for want of memory"), built from
  the recorded reading and never from the log. A pass is a pass whatever the
  readings; a reading that could not be taken is judged as if the rule did
  not exist; the build records its readings as evidence on its attempt and
  is never made unresolved by them. A worker can provoke both causes (it
  can write until the disk is low, or allocate until the kernel kills it),
  so they only ever hand a failed step to a human and never accept
  anything: an unresolved step accepts nothing, and every gate still runs
  on what a resume produces.
  `model_unavailable`, `provider_quota` and `meter_denied` are read from
  the spend meter's ledger of the step's last launch, which the meter
  writes outside the sandbox from each response's head: the model route
  did not serve the step when no request got an answer whose usage was
  read, or when the requests since the last such answer were refused by
  the provider (HTTP 429: `provider_quota`; another failure status:
  `model_unavailable`) or by the meter itself at a ceiling, a window budget
  or the request rate (`meter_denied`). A request that simply got no
  response, or that the upstream rejected as malformed, neither counts as
  a refusal nor ends the count, and a step all of whose requests were
  rejected as malformed has no cause: that is the client's doing. So a
  route that stops accepting connections after it answered is not seen. A
  ledger written before the meter recorded how requests ended names no
  cause. They apply to two steps: a build that exited without succeeding
  (its Activity then fails with the cause's type, the run halts and keeps
  its worktree, and no corrective build is spent), and a review all of whose
  failed gates gave no verdict (below). A challenger job that ends so hands
  over no test: the run goes on without a challenge and the pull request
  names the cause (SC-022).
  A worker chooses its requests and can provoke each of these, so they
  only ever hand a step that did not pass to a human; they accept nothing
  and reject nothing. `no_verdict` is such a review for which the ledger
  names no route failure. A review gave no verdict when nothing was
  flagged or blocking and every review gate that failed did so for want of
  an answer: a criterion went unanswered for spec conformity, the reviewer
  returned no findings list for code review. Its run stays quarantined and
  is kept for a resume, with no quarantine check on the request and no
  corrective build. A review launch that exited with not one request
  served, by the route's or the provider's doing (`model_unavailable`,
  `provider_quota`), is launched once more inside the same Activity,
  within `-verify-max-attempts`; one that was served at all, and one the
  meter refused, are not, and a second launch the Activity's deadline
  leaves no room for leaves the first launch's result standing. The review
  script asks again only for an answer that arrived incomplete, never
  after a turn that failed. A resumed run whose adopted worktree is clean
  at the result commit the adopted run recorded, that run's last build
  attempt having exited 0, launches no build: the build step returns that
  build, recorded with the image and harness it was made with, and every
  gate and the review run on that commit. Only a run that ended (a
  quarantine) records a result commit; a resume of a run an Activity
  failure halted builds as before. An operator's override of a run to
  halted removes its worktree only while no other run's isolation marker
  claims it.
  `route_credential` is read on the host before any launch, by the
  Worker's credential resolver and the sandbox runtime, and never from a
  model's or the worker's output: the route's credential could not be read
  from its source (a missing or unparseable file, an unset variable), it
  holds no value the route's access needs, or it expires before the step's
  budget ends (a chatgpt-codex token within 6 h of its expiry). Each
  observing site puts one sentinel in the error's chain
  (`sandbox.ErrRouteCredential`), and route selection adds it only when
  every route of the role's model was skipped for that reason; a key unsafe
  for its upstream and every other configuration fault do not carry it.
  No model request was made with the credential, so the step gives no
  verdict: a build or review Activity fails with its own non-retryable
  type, the run halts with the cause and keeps its worktree, and a ticket
  build refused before its run existed, a corrective, PR-review or restack
  round refused before its run existed (uncounted, as any unresolved
  round), and a spec-drafting, read-back or planning job
  wait in `resume_review` with the cause instead of halting, quarantining
  or starting again on the next poll. A refused resume of a kept build
  keeps that build as the one `resume` continues. The read-back otherwise
  never stops its request, and a resume after it reruns the whole spec
  drafting pass.
  A spec-drafting or planning job whose sandbox does not start
  halts its request. The run halts with the cause
  as its `HaltReasonCode`, a ticket run keeps its worktree (one built on an existing branch
  keeps none and can only be rebuilt), and the request waits in
  `resume_review` with the cause; a corrective or PR-review round
  that ended so is recorded with the cause, and the next round is not
  started on its own. A sandbox that never started its command is launched
  again inside the same Activity, up to `-verify-max-attempts` launches in
  all, for the review, full-suite and repository-gate steps: nothing ran, so
  nothing is spent twice. On those steps a command that started is never
  launched again, but for the one `dependency_fetch` rerun of a full-suite
  or repository-gate command and the one flaky-test rerun below. The build (`-build-app-max-attempts`) and canonical
  verify (`-verify-max-attempts`) launch again after any launch error
  except an unconfirmed removal, a command started twice or a reached
  ceiling. Temporal's `MaximumAttempts: 1` stands. A run that halts for any
  of these causes counts toward a repository's stop line as an infrastructure
  failure does, except `worker_lost` and `temporal_unreachable`, which do not
  count. An
  unresolved step accepts nothing: every gate still runs on what a resume
  produces.
  A canonical verify, full-suite or baseline verify command that exits
  non-zero with a failure that looks like a flaky test is run once more in
  the same Activity, and that second run's exit code is the step's result,
  pass or fail. The failure qualifies only when its log names between one
  and three failing tests (go test or pytest) and nothing else (no file
  that did not compile or import), and holds no race-detector report,
  `panic:` or `fatal error:`; for canonical verify and the full suite, only
  when the run's record proves the commit the ticket's work started from
  (as for a gate's rerun on the base commit) and the change mentions no
  failing test: neither the test's name nor the name it tests (the part
  after `Test`, `Example`, `Benchmark` or `Fuzz`; a pytest function's name
  without `test_`) is a word, in any letter case, of the path or of either
  version of any file that differs from that commit (committed,
  uncommitted or untracked; of a documentation file, which no test runs,
  only the lines the change added or removed, and the whole file again
  when git shows it no text hunk), no failing pytest file is one of them,
  and no changed path is a submodule; for the baseline verify, only when the
  failure would halt the run. The worktree is put back to exactly what it
  held before the first run, or the rerun is not made: every path the first
  run added is removed, ignored files included, every tracked file it
  changed is restored, and no file the build left uncommitted may have been
  changed by it. The launch path, setup commands, `.factory/` mount and
  limits are the first run's, its one `dependency_fetch` rerun included,
  and no model is called. The first run's result is checkpointed before the
  rerun with the rerun recorded as not run, so a worker lost mid-rerun
  leaves the first failure; a rerun whose launch fails fails the step as a
  first run's launch error does. The rerun is not made when less than twice
  the first run's duration, plus a minute, is left of the Activity's time.
  Both logs are kept, the rerun is the step's last attempt, the run records
  it on the check's result (`rerun`) or on the baseline record, and a pull
  request's risk header names every check that passed on its rerun, with
  the tests that failed the first time, and is never LOW. Limit: a change
  that makes a test it does not mention fail only sometimes (a race the
  detector did not report, in code the test reaches) can pass on its
  rerun; the person who merges sees that it did.
- **SC-018 — only a build is told about an earlier attempt, and only from
  the factory's record:** a ticket run saved quarantined or halted leaves a
  handoff (`handoff.json` in its run directory, its SHA-256 on the run
  record): its rounds, each failed check with the factory's own sentence
  about it, the failing lines of a command gate's output, and the bin of
  each check. Every value in it that a build, a repository command or a
  review model wrote is one line, cut to a fixed length and stripped of
  escapes, backticks and recognisable secrets when it is built, and the
  operator's triage sentence is not copied in. It is used only when the file's hash is the one the run
  recorded and it describes the state the run is in now. It, and the
  notes in it, reach a model in one way: rendered as text and staged read-only for a later
  build attempt of the same ticket (`-earlier-attempt`, the build script's
  `--earlier-attempt`), which puts it in that build's first prompt. It is
  never part of a spec, never an input of a review, challenger, verify or
  gate step, and never given to a planner, another ticket or a pull
  request. A challenge that failed (SC-022) reaches its advice round's
  build the same way, in a record the factory builds from the challenge
  record and never saves as a handoff: each failing challenge file's path
  and the criterion its header cites, and, for each failed test, its name
  and assertion in at most 20 lines, each one line, cut and cleaned; the challenge files'
  content is never in a prompt, and the build reads them only from their
  read-only held copies. A required CI check that failed on the pull
  request (the paragraph on CI checks below) reaches its CI round's build
  the same way: the check's name, its conclusion and its details URL, and
  the last 200 lines of the failed job's log, at most 16 KiB, each one
  line, cut and cleaned,
  quoted as data. An accepted run's `high` review findings reach its advice
  round's build (SC-022) the same way, in a record the factory builds from
  the run record and never saves as a handoff: at most five, each value
  (severity, file and line, summary, failure scenario) one line, cut and
  cleaned. It is
  not left where a later step of the same run works: the build script
  never writes it into its round-state file, nor the notes into its
  round-state, evidence or report files, its output or the progress feed,
  and the build's harness
  session, which holds its prompts, is removed from the worktree when the
  build step returns (its rounds' saved output and the prompts it saved are
  copied to the run directory first, where only the operator's prompts route
  and `factoryd logs -prompt` read them); a session that cannot be removed, or is not a plain
  directory, fails the build step, so no later step runs beside it.
  With the session key `round_session_continuity` on (on by default), a
  request ticket build's session is first also copied to the run directory
  (`build-session/`, `keepBuildSession`, while the step heartbeats): read
  and written through an `os.Root` on each side, every file opened with
  `O_NOFOLLOW|O_NONBLOCK` and copied only when the opened handle is a
  regular file, no link, never its saved prompts, its rounds' output, the
  agent's notes, a harness's rendered configuration (`config.toml`,
  `config.json`) or skills folder, or a file named `auth.json`,
  `.credentials.json`, `.tmp` or `tmp`, at most 64 MiB read through a limit,
  10,000 entries and 32 levels, or nothing but the reason. The notes turn
  sets the session's state aside and puts it back after, so the session
  never keeps the notes; one whose notes could not be taken out is marked
  and never continued. A corrective or advice round of the same ticket, and
  nothing else, is given that copy of the run it follows
  (`-resume-session-of`, refused unless the round is a request ticket build
  and the named run is of the same request, ticket and workspace): the build
  Activity alone copies it, under the same rules, into the
  round's worktree session folder before the build and passes
  `--resume-session`, and the build script continues it only when it was
  made by the same harness and model, holds at most 32 MiB of session state
  and the harness can continue it, else removes it and starts fresh with
  the written record (`session` in `BUILD_EVIDENCE.json` says which). That
  session holds what the build it follows was told, an earlier attempt's
  record included: it reaches only a later build of the same ticket, as the
  record itself does, and is removed from the round's worktree when its
  build step returns like any build session. No review, verify or gate step
  is given it. The copy in the run directory is removed (`PruneBuildSessions`,
  through an `os.Root` on the runs directory: the directory itself only,
  never through a link) after each worker step of the request and on
  cancel: the sessions of a ticket lower than the one a building or
  `resume_review` request is on, and every session of the request in any
  other state, except one a round still in flight follows (a run of the
  request not yet terminal names it as `EarlierAttemptOf`, or a ticket's
  advice round not yet started names it). A request that stops outside a
  worker step or cancel keeps its sessions.
  A line the worker prints in the progress feed's protocol can never carry a
  `log` path or begin or end a start-up step: the feed drops a log from every
  line but the factory's, and the worker parser accepts only the `round` and
  `agent` stages.
  The build's evidence file (`BUILD_EVIDENCE.json`) and round-state file
  (`.pi-build-round-state.json`), which hold the build agent's last message
  of each failed round (`agent_notes`), leave the worktree with the session:
  the host moves the evidence file to the run directory, where the run
  record is read from, and removes the round-state file, which a finished
  build no longer needs; whatever is at either name is removed, a directory
  with all it holds and no link followed, and only a removal that fails
  fails the build step, so no review works beside it. A build that was lost keeps both for
  its resume, and no review follows a lost build.
  A saved prompt is what the build's session folder held when the host
  copied it; a build can alter its own before that, because the build script
  and the coding agent run as one user in one sandbox and the script has no
  channel to the host that the agent cannot also write (its output file sits
  in a directory the sandbox writes). No second user can be given to the
  agent (the sandbox's seccomp filter denies every set-id system call), a
  child of the same user can open its parent's descriptors through `/proc`,
  and every directory the host shares with the sandbox is shared with all of
  its processes. So the host verifies no saved prompt
  and attributes no line of the output to the script: a `FACTORY_PROGRESS`
  line is recorded as the worker's (`source: worker`), with the host's time,
  in the `round` and `agent` stages only, is shown and never read by a gate,
  a policy or a state transition, and cannot be recorded as one of the
  factory's own lines. The operator surfaces label a saved prompt "as saved
  by the build", never "sent". Before a build or a review is launched the host
  removes whatever its session's prompts folder holds, making a folder left
  read-only removable first; a folder that cannot be shown gone fails the step
  as an infrastructure error, so no launch runs beside a prompt it did not
  save and a file an earlier step planted is never kept as a later launch's
  prompt. What the build agent itself chooses to write into the
  workspace is an ordinary part of its diff, judged like the rest.
  A build that ends without passing, or that passes after at least one round
  that did not (never one that passes in its first round), is asked once, in
  its own session, for notes under five fixed headings. The turn after a pass
  runs with the harness's tools, so the build script, not the prompt and not a
  harness flag, keeps the tree: it records the worktree before the turn and
  again after it (every path outside `.git` and the session folder, tracked,
  untracked and ignored: kind, permission bits, and the SHA-256 of a file's
  content or a link's target), and keeps the reply only when the two records
  are equal. Otherwise it removes the reply and puts the tree back only if
  every difference can be undone exactly from what it holds (all or nothing:
  one changed file with no intact copy, and the tree is left as the turn left
  it, nothing deleted or moved); a third record equal to the first leaves the
  build passed,
  and only a tree that cannot be shown equal has the build's own setup
  and verify run on it again: if they fail the build ends as not
  passed, its last round recorded as failed, and no further round runs, so one
  build runs the turn at most once. A tree too large to record is not given
  the turn, and a launch lost during the turn is resumed with no further round
  or turn and its tree checked again. What the records show is the tree at the
  moment of the last one, no more: a process the turn left running can write
  afterwards, exactly as after any build round. This is the build script's
  own claim inside the sandbox, where it and the agent are one user, and the
  host relies on none of it: it commits the worktree only after the build step
  has returned, and its own verify, gates and reviews judge that commit
  exactly as for a build with no notes turn. The notes file exists when the
  build script ends only if this launch's notes turn gave a reply, which the
  script wrote redacted and cut: on every way out of the build, after its
  last agent turn and whether the notes turn ran or was skipped, whatever is
  at the path (a file, a link, which is never followed, a directory) is
  removed first, and a path that cannot be cleared skips the turn. The
  factory copies the reply out of the
  session folder before removing it, splits it into one-line items cleaned and
  capped like every other value, and stores them in the handoff apart from its
  own facts and after them. They are labelled the agent's view, never decide a
  bin, are absent from the run record and every MCP tool, and are otherwise
  readable only through the operator's own handoff route, or, inside a saved
  build prompt that carries them, the operator's prompts route. The items
  under the fifth heading (things worth knowing about the repository) may
  also be listed to the operator as memory candidates, each only if it
  passes the memory text rule; they still reach no review, no planner and no
  other ticket's build except under SC-020. The memory list does not take
  them from the handoff: for a run saved quarantined, halted or accepted, the
  items under the fifth heading are read by the operator's memory list from
  the host's copy of the notes in the run directory (`agent-notes.md`), as
  written, each one line cleaned and cut like every other value and a
  candidate only if it passes the memory text rule, which accepts a command
  only inside backticks and repairs nothing. That file's text has two readers
  and no other: the build of the handoff, which stores every item with its
  backticks turned into quotes (and through it the handoff route and a later
  build's record), and the memory list, which reads the fifth heading alone.
  A run saved accepted has no handoff, so the rest of its notes reaches no
  reader, the handoff route included.
  A ticket rebuilt after `factoryd retry` is given the record of its own
  quarantined run on the same terms, and only while the ticket's spec is the
  one that run was built from.
  That rebuild continues on the quarantined run's branch, from its commit,
  only when it is given the record, and its gates, reviews and release
  decision then judge the ticket's whole change from that run's diff base;
  a run it is given no record of is never built on, and `retry -from
  scratch` and `resume -from scratch` rebuild from the base in every case.
  A build that follows one which was given the record and never finished
  (lost and resumed in its kept worktree, or halted and built again) is
  given the record of that same quarantined run: the unfinished run names
  it (`earlier_attempt_of` in its run record), and the record is rendered
  again from that run's handoff under the same hash, state, request and
  spec checks, never copied from what the unfinished run was handed.
  A corrective build follows a quarantined ticket run only when the handoff
  sorts every judged failed check as one a build can fix when told about it:
  `canonical_verify`, `full_suite_verify`, a repository's own gates,
  `diff_scope` and `tests_red_on_base`. A review that gave no verdict (when
  only the reviews failed, the request waits in `resume_review` and the run
  is no quarantine of it), a
  repository gate that never ran, a build that a repository setup command
  stopped before its first agent turn (by the meter's count of zero and an
  unchanged commit, never by the sandbox's exit status or log), a
  repository gate that failed the
  same way (the same exit code and the same whole output, after removing
  elapsed times, addresses and the like; output that is empty or cannot be
  compared never counts as the same) when
  the factory reran it on the commit
  the ticket's work started from (no build can make it pass; the rerun is
  evidence on the gate's result and never changes whether the gate passed; a gate already failing there in
  another way stays one a build is told about, with a factory-written
  sentence saying so) and any check with no bin never do,
  and a halt never does. A check on the diff of an attempt that committed nothing
  (canonical verification never passed) is not judged either way. Such a
  build is a new run on the quarantined run's branch, started after the
  quarantine was recorded and notified (SC-013): every gate runs again on
  its result, it counts against `review_corrective_rounds` (one budget per
  ticket build, shared with the review round) and the launch budget, and
  it changes nothing about who merges (SC-001). A round whose run never
  started, halted with no verdict on the work (SC-017's causes), or was
  quarantined only by reviews that gave no verdict, counts
  against neither `review_corrective_rounds` nor `max_review_rounds`: it
  says nothing about the work. After the second and third kinds no further round
  starts until a human decides in `resume_review`, and each launch still
  counts against the launch budget.

The `tests_red_on_base` check (a change must add or change a test that fails on the base commit and passes on the result commit) runs the changed test files in a gate sandbox on scratch worktrees of the base and of the result commit, by the launch path of SC-022: the result commit's copy is held on the host, hashed and mounted read-only, and each file's SHA-256 and launches are recorded on the run (SC-008). It judges only the change's own test IDs: an ID the result commit's copy of a changed test file holds and the base commit's copy does not, or one whose span in that file, as the runner's host parse of the file gives it, holds a line the change added or removed. Every other test of the file may run and is not judged. No size or test-count cap applies to a test file the base commit already holds. It refuses only when every judged test ID passes on both commits, or when no test file changed and the ticket declares no `Tests-Required: no -- <reason>`; it never refuses without a verdict. A changed test file it does not run or judge (no runner for its path, a host parse that fails, no judged test ID in it, a cap on an added file) is recorded on the run with that reason, read from what the factory observed, and the pull request's note names the file and its reason. A bug request's (`submit -bug`) first ticket has no opt-out: its plan is refused when that ticket declares `Tests-Required: no`, and the check refuses its run unless a judged test ID fails on the base commit and passes on the result commit; a judged test that gave no verdict passes with a note there too. An unrunnable file, a missing report, a changed test that failed alone on the result commit after the full suite passed, or an unknown base commit passes with a note in the pull request.

The `diff_scope` check refuses a run only against a list of files that a person approved, or against the repository's own list. A ticket's `Allowed-Files:` is such a list when a person approved the plan it belongs to at `plan_review`, when the operator handed the plan over (`submit -plan-dir`) and no plan-stage feedback has ended the hand-over, when the operator widened it with `factoryd amend-scope`, or when the operator wrote the ticket for a direct `factoryd run` with no request. The one ticket the factory writes from a spec submitted without a plan, and approves itself (`ApprovePlanByFactory`), carries no `Allowed-Files:`, so `diff_scope` judges it only against the repository's own list; a plan the planning job drafts after a size refusal always waits at `plan_review`, so its lists are approved by a person. `.factory.yml`'s `allowed_paths:` is the repository's own list, written by a person, and always refuses: a changed file that matches none of its patterns fails `diff_scope` whatever the ticket says, with the same harness by-products and the factory's committed challenge files (by their pinned SHA-256, never by path; SC-022) set aside as for a ticket's list. `release_protected_paths` and the built-in protected paths (`.factory.yml`, `.factory/`, `.buildgate/` and the CI definition paths below) are separate checks over the same changed files and are not affected. A baseline verify's leftover halt and the preflight's misprefixed-path halt follow the same rule: neither refuses against an unapproved list.

A pull request's CI checks are read on every PR-review poll from the forge's status check rollup of the PR head (check runs and commit statuses: each one's name, the app or context that posted it, its status, its conclusion and its details URL) and whether each is required for the pull request (`isRequired`, read per check from GitHub's GraphQL API). Only a required check that completed with conclusion `FAILURE` on a PR head the factory pushed (the `ResultSHA` of the ticket's `Ticket.PRHeadRunID`) starts a CI round. A check not yet completed starts nothing, and the request's next action names it as what the request waits for. A check that is not required and failed is one note on the ticket and in `factoryd watch`, never a round. A required check whose failure is not the code's starts no round and sends the operator a triage sentence naming it: a conclusion other than `FAILURE` (`CANCELLED`, `TIMED_OUT`, `STARTUP_FAILURE`, `ACTION_REQUIRED`, `STALE`), or a check posted by an app or context on the factory's fixed list of checks that run none of the change's code (CLA and DCO checks). That sorting reads those structured fields only, never a log line, so a failure only its log shows not to be the code's (a missing secret) is treated as the code's. When whether a check is required cannot be read, every check that completed with `FAILURE` counts as required, the request's next action says the required set was not read, and still at most one CI round starts per check per ticket. A CI round is a PR-review round (the Forge row): one build on the ticket's PR branch with the ticket's unchanged spec, given the failed check as its `-earlier-attempt` record (SC-018), judged by every gate and a fresh review, pushed only under the Forge row's checks, and counted against `max_review_rounds`; it changes nothing about who merges (SC-001). When the same required check completes with `FAILURE` again on the head a CI round for it pushed, no second round starts: the request moves to `resume_review` with a triage sentence naming the check and no unresolved cause (`pr_review -> resume_review`), and `factoryd resume` reads the checks again.

The CI definition paths are built-in protected paths, matched in any letter case and never exempted, as `.factory/` is: `.github/workflows/`, `.github/actions/`, `.gitlab-ci.yml`, `.gitlab/`, `.circleci/`, `.buildkite/`, `.travis.yml`, `azure-pipelines.yml`, `.azure-pipelines/`, `bitbucket-pipelines.yml`, `Jenkinsfile`, `.drone.yml`, `.woodpecker.yml` and `.woodpecker/`. A run that changes one is denied release, and only an operator's override (SC-007) releases it.

The size of a change is measured on the host from git, after every gate of a request's ticket build but the code review has passed, and before that review and the challenger (SC-022) run, over the run's diff from its diff base to its `ResultSHA`: source lines changed (insertions plus deletions, `git diff --numstat`), source files changed, and functions changed (the distinct enclosing functions git names in the hunk headers of `git diff -U0`, with git's built-in function patterns chosen by file extension from a fixed host table). No count includes a file that matches `test_patterns` as read from the commit `.factory.yml` was read from, a dependency lock file, a generated file on the factory's fixed list, documentation (a fixed list of extensions, and anything under a `docs/` directory) or a binary file. Git reads attributes from the empty tree (`--attr-source`), so no `.gitattributes` of the run's commits selects a diff driver or marks a file generated. This is the one size measure: the release hard caps (`release_max_changed_lines`, `release_max_files_changed`; Release-policy defaults) read the same source-line and source-file counts. The numbers decide which way the pull request opens:

| Measured | Pull request |
|---|---|
| At most 400 source lines and at most 25 functions | Opens as before |
| Otherwise, within both release hard caps (by default 1000 source lines, 25 source files) | Opens with one note in its body that gives the numbers |
| Over either hard cap | Does not open: the request goes back to planning |

A request whose ticket measures over either hard cap opens no pull request: the run is not reviewed or challenged, it ends with its release decision denied for size, and the request moves to `planning` (`building -> planning`, below) with the measurement (each file's counts and the names of its changed functions, numbers and names from git only) and the run's diff, given to the planning job as fenced data and never as instructions. The planning job drafts stacked tickets cut where the measurement shows the change separates; that plan always waits at `plan_review`, and each of its tickets is built from its own base, judged by every gate, measured again, and opens its own pull request. A ticket of such a plan, or of a plan handed over (`submit -plan-dir`), that measures over either hard cap opens no pull request and halts the request with the measurement, never moving it to `planning` a second time. A single-ticket `factoryd run` outside a request is measured the same way and has no planning step: over either hard cap its release decision is denied.

A repository can require that parts of a change ship as separate pull requests, in order, with the `.factory.yml` key `pr_order`: a list of path groups, each a list of patterns in the syntax of `allowed_paths`, read from the commit `.factory.yml` was read from, never the result commit. A changed file belongs to the first group with a pattern it matches, and a file that matches none belongs to one more group after the listed ones. The check reads changed paths from git only, never a file's content or prose in an instruction file. A rule written only in prose (an `AGENTS.md` sentence) is the build's instruction and is not checked. When a request ticket's accepted run, after its challenger (SC-022), changes files of more than one group, it opens no pull request: the host carves its result commit into one commit per group it touches, in group order, each a child of the one before, the first a child of the run's diff base; carved commit k's tree is the diff base's tree with the changed files of the first k touched groups as the result commit holds them, so the last carved tree is the result commit's. The ticket is replaced by one ticket per carved commit, which the factory writes and approves itself (the order is the repository's own rule, so no plan waits at `plan_review` for it), each holding the ticket's approved spec and the name of its group. Each is built, stacked on the one before, by a replay run of its carved commit (no model build): every command gate (`canonical_verify`, `full_suite_verify`, the repository's own gates) runs on its tree, the code review runs on its diff, and no challenger runs; `tests_red_on_base` and `diff_scope` are not run again, having judged the whole change on the run that was carved. Before carved ticket k is built, when the head of ticket k-1's branch is not the commit k was carved onto (an advice round moved it), the host makes k's commit again on that head from the carved run's result, if that head changed none of k's files, and builds that; a head that changed one of them parks the request in `resume_review` with the reason, and a stale tree is never built. Each carved ticket opens its own pull request, stacked, with its own size note when its own diff is over the note band. A later build of a carved ticket (a PR-review, CI or advice round) is held to its group: a result whose diff from the ticket's stack base changes a file of another group is denied release. A replay run of a carved commit that any gate refuses sends the request to `planning` (`building -> planning`) with the run's diff, the measurement and the groups, as for a size refusal. The planning job is told the groups; a ticket of the plan it drafts, or of a plan handed over (`submit -plan-dir`), that changes files of more than one group opens no pull request and halts the request, never carved and never moving it to `planning` a second time. A single-ticket `factoryd run` outside a request ignores `pr_order`.

- **SC-019 — a review reads the base commit's instructions:** every model review of a run, and its challenger job (SC-022), launches with the repository's instruction files (AGENTS.md-style files, harness folders, project skills, MCP and hook configuration, at any depth of the repository) as the commit the request started from holds them (for a single-ticket run, the run's own base), mounted read-only over the worktree; what the build changed in them reaches the reviewer only as quoted data; an instruction-named path the worktree holds beyond the result commit is removed before the review; a tracked instruction file that no longer matches the result commit, a link from an instruction path to a directory that is not one, a checkout-converted instruction file, or any other shape the mask cannot carry halts the run before the review launches, and a build is never told why. Each review attempt and challenge record records the snapshot's SHA-256, the masked paths and the removed paths.
- **SC-020 — memory reaches another ticket only approved or merged, and only inside the fence:** a line derived from one ticket's runs (a build agent's note that passed the fixed text rule, rendered by the host as one list line) reaches a model working on another ticket in two ways only: inside the spec and ticket of a memory request, which stops at spec review until the operator approves it, or as the fenced section of root `AGENTS.md` at the commit that work starts from. A run of a memory request is released only when its result commit holds exactly one root path whose name folds to `AGENTS.md`, spelled `AGENTS.md`, a regular file of mode 100644 that is byte-for-byte the text the factory rendered when the change was proposed, and the run changed no other file; and only when root `AGENTS.md` at the run's diff base is the file the proposal was rendered from (its SHA-256 is the proposal's `base_blob_sha256`, or neither has a file), so a change a person made to the file after the proposal is never overwritten: the release is refused with `AGENTS.md changed after the memory change was proposed; propose it again`, and the worker halts such a request with the same sentence before its build when the checkout's HEAD already differs. Once a repository's root `AGENTS.md` holds a memory section (at the run's diff base, a root file of that name in any letter case holds the text `buildgate:memory`), a run that is not a memory request's may not change that file in any way (its text outside the markers, its mode, its type, a move or a delete) nor add a case variant of its name; before a section exists, such a run may change or create the file but may not leave the marker text in it (written plainly or as HTML character references), a second spelling of its name, or a symlink, submodule or directory in its place. The host reads this from git objects, never the worktree, records it on the run, and refuses the release on any read failure; the operator's override computes it on the host before its decision, and the HTTP override refuses a run that lacks it. With pull requests on, a ticket whose release decision is not allowed halts its request before a later ticket is built on its commit; with pull requests off nothing is pushed or opened, the tickets are built in order and each run's decision is on its record. A memory request changes at most five lines, and when the section is full the proposal is refused: the factory never chooses a line to drop. Candidates are shown only to the operator (`factoryd memory list` and `show`, the project's memory route); nothing collects, proposes or drops one without a person running the command, the factory never runs a command a line names, and a reviewer's comment on a memory request's pull request is answered and starts no build. The section is read only when every line of it is free of control and format characters, and a file with no section is given one only when it does not end inside a code fence: whatever the factory accepts, the file it renders reads back. Memory is off for a repository until the operator lists it under `memory.repositories`, and the project's kill switch or `factoryd memory off` stops every memory action that changes anything. Of the three switches, two stop a memory request already in flight: its run is not released while the kill switch is engaged (as for any run) or while the store holds the off marker `factoryd memory off` wrote (`repository memory is switched off (factoryd memory off)`). Removing a repository from `memory.repositories` stops new proposals and every other changing subcommand, and does not stop the release of a request already in flight.
- **SC-021 — a repository's install runs only as approved, and its cached result comes only from a trusted commit:** a run whose committed .factory.yml has a sandbox: block launches no sandbox unless the operator approved that block's SHA-256 for that repository; a reused install image was built from the commit .factory.yml was read from, keyed by every InstallImageKey input, and holds nothing a build, verify or gate sandbox wrote; a run whose starting commit's install inputs differ uses a run-local image no other run reuses. The key covers the image format, `Dockerfile.install` and the restore step, the scope, the repository, the block's hash, the configured and the effective sandbox image (both pinned by digest, else refused), the worker's uid:gid, the narrowed registry proxy routes and the proxy image, and the path and blob id of every dependency manifest, lockfile, package-manager setting and version file (and the block's `inputs`) at the commit the install ran on, outside `vendor/`, `testdata/` and `node_modules/`. The image's tree holds only regular files, directories and links that resolve inside their cache path through the tree's other links, and is launched by the digest the local registry serves for its key. The run records the image as `Run.Install`; an accepted result whose input carried the block's hash and that has no `Install` record for it is quarantined (`InstallNotRunMessage`).
- **SC-022 — a challenger's test refuses nothing and starts no round of its own:** After every gate of a request's ticket build before the code review has passed, and its size is within the pull request bands (the paragraph on size after SC-018), the ticket's challenger job runs once, started with the code review on the same result commit where Docker's memory holds both at the limits (`max_parallel_jobs` × 2 × `sandbox_memory`), else after it, the order recorded on the challenge record, and never given the review's findings, nor the review given its files: the first such run of the ticket is challenged, and no later build of the ticket starts another challenger job. The job is a sandbox launch of the `challenger` role (SC-015, SC-016) on that role's model through its own route, with the run's image, registry proxy, setup commands and `.factory/` mount, on a scratch worktree of the run's result commit under the run's directory, discarded afterwards: it reads the whole tree and may write and run tests there, but it is never the run's own worktree or branch, and nothing it writes reaches either except as below. That it tests the change's public surface is an instruction in its prompt, not a containment. It reads the base commit's instruction files (SC-019). It is given the approved spec (re-hashed against the hash `spec_review` approved), followed, unless every criterion is labelled `AC-<n>` with its own number, by each criterion the factory numbers with its ID, the read-back answers and the list of the diff's changed paths, and no build session, handoff, agent notes, review finding or build prompt (SC-018). It may run its tests in its own sandbox before it hands them over; nothing it reports is read as a verdict. A single-ticket `factoryd run` outside a request has no approved spec and runs no challenger. What the challenger hands over counts only through the factory's copy. The host copies out once, created exclusively under the run's `challenge/` directory with mode 0444, each file that is a new relative path matching `test_patterns` (or the global defaults when unset) as read from the commit `.factory.yml` was read from, with no `..` component and no link among its parents, not equal under case folding to a path tracked in the result commit, not an instruction path (SC-019), not under `.factory/` and not a protected path, within the size cap per file and the cap on the number of files. Its SHA-256 is recorded on the challenge record and checked before each mount; its content is never recorded there. A file whose first lines carry no derivation header (one fixed header line naming one numbered acceptance criterion of the approved spec, `AC-<n>`, or one numbered read-back answer, that the spec or the answers hold) is held, not run, and is one note in the pull request; the header is read as a label and a number, never as prose. A request ticket's challenge may cite only a criterion that the ticket or an earlier ticket of the request lists under its `### Acceptance criteria covered` (or an answer): a file whose header cites any other criterion is held and not run, with a note, and the challenger is told the covered criteria; a ticket whose own coverage cannot be read is not narrowed. A file the shape rules refuse is a note naming its rule. The factory's run of the held files is a gate sandbox launched by the same path as every other gate (the run's image, network policy, setup commands and `.factory/` mount, no model route, no credential) on a scratch worktree of the result commit, discarded afterwards and never the run's own worktree, with every held file that carries a header mounted read-only at its path. Where a reproduction runner covers the file's path (`internal/reproduction`: Go, Vitest, pytest, Maven Surefire, Minitest), chosen in the file's nearest enclosing directory, the repository root included, whose marker files (read from the judged commit) a runner accepts, its command run in that directory, and reads test IDs in it (a Vitest test inside a suite called through `.each` or `.for` has none: its name is known only at run time), the command is that runner's, selecting only the held files' own test IDs with the factory's per-test timeout, and the verdict is the runner's structured report for those IDs: every ID passed is green, a failed ID (an assertion, a panic or exception, the per-test timeout) is red, and a compile or collection error naming a held file, or a missing or unreadable report, is a broken challenge: a note in the pull request, no round. Any other file runs under the repository's own `verify_command` from the committed `.factory.yml`, all such files in one launch, whose exit status alone is the group's verdict (0 green, any other red), recorded on each file as the group's (`group`) and told in the pull request and a round's record as one run that failed, never as a failure of each file; the pull request notes that its collection is not proven. A red verdict is run once more as a separate launch before anything follows from it, and counts as red only when both launches are red; a red then green is advice, no round. A launch whose sandbox never started the command, a copy, hash or mount failure, and a challenger job that handed over no file, whatever its exit status (SC-017's causes included; the files a job left are held whatever its exit status, with a note when it was not 0), are no verdict: the run goes on without a challenge and the pull request names the cause; nothing halts. Each launch counts against the launch budget and the Activity's time (SC-009); its exit status, its log and the SHA-256 of every file it ran are recorded on the run (SC-008). Green, with the code review's verdict given (a review with no verdict commits nothing, keeps the challenge record on the run, and the ticket is challenged afresh by the build that follows the resume): the factory's one host step that commits pinned files onto a run's branch commits the held files as a child of the result commit, re-reads each committed file from that commit and compares it with its pinned hash, and that commit is the run's `ResultSHA` for the release decision and the pull request, once the run's `verify_command` has passed on it in a fresh sandbox (the held files' own test IDs do not cover a table row, a type error or a vet failure); a verify command that fails there, or a verify step that errors, hard-resets the worktree and branch to the previous result commit (only when HEAD is the challenge commit and its parent that result commit; the run's deadline allows for this one verify), leaves `ResultSHA` and the changed files as they were, makes the verdict no verdict with the failure as its cause and a note in the pull request, and a reset that fails halts the run; `diff_scope` sets the files aside by their pinned hash, never their path. Red: an open item of the ticket's advice rounds below, once its pull request is open; with no pull request open, and for a run `pr_order` carves into several tickets, no round runs, the red files are listed in the run record, and the pull request body lists them when one opens (a carved run's, on the first carved ticket's pull request, with a note that no round ran). A round given red held files has them mounted read-only where the build can read and never write them, is told to fix the code or decline with a reason, and runs them again on its result as above; while they are not yet committed on the branch, a round whose held files are all green commits them as above. A held file still red after the last round is advice: the pull request lists each red challenge file's path, the criterion it cites and the build's reason, one line each, cut and cleaned as SC-018's values are, never the file's content; the body is written when the pull request opens and not edited afterwards, so a red list names the build's result commit it judges and says the section is not updated for later commits on the branch. A challenge never quarantines, halts or parks a run or a request, and is never a failed or ambiguous policy check under SC-013; a round's push that failed once sent halts the request whatever the round was given (below), because the commit may be on the remote. Every later build of the ticket (a corrective, PR-review, CI or advice round, a retry, a resume) runs the held files again on its result by the same path; that result starts a round only as an open item of the advice rounds that follow an accepted ticket run (its first build, or an accepted corrective round), and is otherwise advice in the pull request. A restack's replay run runs no challenge. It is given the held files of the challenge its branch may carry as set-aside only (`-challenge-set-aside`, refused without `-replay` and with `-challenge`, `-challenge-held` or `-challenge-round`): those of the ticket's own challenge run, or, for a carved ticket, of the run it was carved from when that run's challenge was committed. `diff_scope` sets a file aside where the branch's commit holds it at its pinned hash, as for every build, and nothing mounts, runs or times them. A model review's finding never refuses a run and never starts a corrective build: `code_review` fails only when the reviewer gave no verdict, and no review writes a test the factory runs. Every finding is listed in the pull request body with its severity and one line, cut and cleaned as SC-018's values are; after an advice round's confirmed push one pull request comment lists, the same way and with its severity and summary only, the findings the round was given and its own review's findings. A review that gave no verdict is not a finding: it hands the run to a human under SC-017 and refuses nothing. A request's accepted ticket run whose pull request is open and that has an open item (a `high` review finding, or a red held challenge file) gets up to `review_advice_rounds` advice rounds (0 to 3, 3 by default; at least one when the accepted run has a red held file) before the request leaves `building`, each one build on the run's branch with the ticket's unchanged spec on the execution role's model (never the escalation), given one `-earlier-attempt` record (SC-018) with the red held files of the run it follows first and then its `high` findings, opening no pull request, judged by every gate and a fresh review, the held files run again on its result; round k+1 runs only after round k's result was pushed and that result still has an open item, never after a round whose run it followed had no red held file and whose own review recorded no fewer `high` findings, and a round runs only while every launch budget has at least the spend of the run it repeats left (its spend counts toward the request's budget). An advice round never refuses: only its accepted result whose held files include none that was green on the run it followed and is not green on it (red, or broken), pushed to the pull request's branch under the Forge row's checks and confirmed, replaces the run it followed as the ticket's run and PR head; any other outcome, a release decision not allowed, or a push check that refuses leaves that run, the remote branch and the pull request as they were, puts the local branch back on that run's commit (the Forge row), and quarantines, halts or parks nothing (a push that failed once sent, or a local branch still ahead that could not be put back, halts the request). While a round builds the ticket's run stays the run it follows. Advice rounds count against neither `review_corrective_rounds` nor `max_review_rounds`. One a stopped worker left in flight (`Ticket.Advice`) is settled from its run by the next pass, never started again, and settled as halted once its run has not ended within a ticket build's step limit and an hour; a worker lost mid-round parks the request in `resume_review` through the lost-step path as for any build, and the round is settled when the request returns to `building`; one still in flight when the request reaches `pr_review` (a halt, then a retry) is recorded as not pushed, once its run has ended (waited for up to the same bound), before any PR head is read.

`factoryd memory add -run <id>` records an operator-cited evidence reference separately from runs whose agent notes said a candidate. It accepts only a confirmed terminal run recorded against the same repository root; the reference is shown only to the operator and does not change the candidate's agent-run count, collection eligibility, or what a model receives. Each list is capped at 20 run IDs, and schema migration preserves existing stores.

### Release-policy defaults

These rules set the starting values `internal/release.MergePolicyCheck`
receives. None of them changes what that check enforces.

- **Presence, not soundness.** `MergePolicyCheck` checks that the rollback
  plan and SHAs are *recorded*. It does not check that the plan is sound or
  that the commits were reviewed, which would need review-attribution
  evidence the factory doesn't collect yet.
- **Default limits.** An empty policy (no `release_max_files_changed`/
  `release_max_changed_lines`/`release_rollback_plan`) denies every PR by
  construction. To avoid that, `quickstart`, `init-config`, and
  `quickstart`'s reuse path default these to 25 files, 1000 changed lines, and
  `git revert the merge commit on main`
  (`internal/sessionconfig.DefaultReleaseMaxFilesChanged` and siblings).
  `release_max_changed_lines` and `release_max_files_changed` read the one
  size measure (the paragraph on size after SC-018): source lines changed,
  insertions plus deletions, and source files changed, so a rewrite or a
  deletion counts as an addition does. A request's ticket over either goes
  back to planning before its review; a single-ticket run over either is
  denied.
  - The reuse path only backfills a missing key, never overwrites one.
  - Operators may tighten, loosen, or clear any of them.
- **Policy snapshot per run.** The effective policy is persisted on the run
  at start (`run.Run.ReleasePolicy`), so later reconciliation evaluates
  against the policy the run started with. A legacy run without the field
  falls back to an always-deny `release.MergePolicy{}`, and the fallback is
  logged.
- **Flag precedence.** `factoryd serve`'s `-release-*` flags resolve
  explicit flag > session config, like every other Tier-1 flag.
- **Deny-all warning.** A policy that can never allow
  (`release.MergePolicy.CanNeverAllow`) is reported ahead of time, as a
  warning from `factoryd doctor` and as `release_policy_warning` in
  `GET /console-config.json`.
- **Full-suite fallback.** `RequiredGates` is `{canonical_verify,
  full_suite_verify}`. When no full-suite command resolves from any source,
  the run's canonical verify command is used as the full-suite command.
  - The gate still really runs that command in the sandbox and must pass.
  - The substitution is recorded (`FullSuiteSource = "verify_command"`) and
    shown in the PR evidence body.
  - The literal value `none` (`-full-suite-command none` / `.factory.yml
    full_suite_command: none`) opts out. The run is recorded as
    `FullSuiteSource = "none"`, is never re-substituted, and is denied
    release as before.

## State transitions

The following transitions are the complete normative state graph up to an
accepted, mergeable state. A transition is factory-owned and must be backed
by durable evidence; an agent report cannot create one. Merge and deploy
themselves are not part of this graph — they are human actions that happen
after it, informed by the recorded decision, never automated.

```text
  draft -> product_policy_check -> architecture_policy_check
        -> program_design_policy_check -> ready -> slice_running
        -> verifying -> slice_policy_check -> accepted
                                                  |
                              +-------------------+-------------------+
                              v                                       v
                         next_slice -> slice_running          merge_policy_check
                                                                       |
                                                                       v
                                                             decision_recorded (terminal)

  {slice_running, verifying, slice_policy_check}
    -- any execution --------------> halted
    -- failed/ambiguous check -----> quarantined
  halted -> quarantined
  quarantined -- policy-approved retry, or an
              authenticated human override --> resumed
```

The diagram is a reading aid. The edge lists below are normative and are
verified line-for-line by `internal/claims/safety_contract_test.go`.

### Normal progression

- `draft -> product_policy_check`
- `product_policy_check -> architecture_policy_check`
- `architecture_policy_check -> program_design_policy_check`
- `program_design_policy_check -> ready`
- `ready -> slice_running`
- `slice_running -> verifying`
- `verifying -> slice_policy_check`
- `slice_policy_check -> accepted`
- `accepted -> next_slice`
- `next_slice -> slice_running`

### Failure, recovery, and delivery

- `any execution -> halted`
- `halted -> quarantined`
- `any execution -> quarantined`
- `quarantined -> resumed`
- `accepted -> merge_policy_check`
- `merge_policy_check -> decision_recorded`

A failed policy check goes directly from execution to `quarantined`, not
via `halted`. This is deliberate (`cmd/factoryd/apply_run_result.go`): an
intermediate `halted` save would open a crash window that leaves the record
stuck at `halted`, where `factoryd override` cannot reach it, since override
accepts only a `quarantined` run. `halted -> quarantined` is the separate
path for a run an operator investigates and confirms should quarantine.

`resumed` is permitted only after a policy-approved retry or a durable,
authenticated human override; a quarantined run never silently resumes. A
failed or ambiguous policy check cannot skip `halted`/`quarantined` to reach
`decision_recorded`. `decision_recorded` is terminal: an allowed/denied
verdict, durable and evidence-backed, with no factory-owned edge onward to a
merged or deployed state.

### Request lifecycle: dismissing a dead end

Separate from the run-level graph above (`internal/run`, one slice at a
time): `internal/request`'s own request lifecycle runs `submitted ->
spec_drafting -> ... -> building -> pr_review -> done`, with `quarantined`
and `halted` reachable from any non-terminal state on a gate or
infrastructure failure (`internal/request.Quarantine`/`Halt`). A
quarantined or halted request could previously only be retried
(`internal/request.Retry`), never dismissed, so a request an operator has
no intention of retrying stayed red in `factoryd doctor`'s mount-visibility
check forever. `internal/request.Cancel` accepts these two states, and every
non-terminal one, as the operator's own distinct end of a request:

- `submitted -> cancelled`
- `spec_drafting -> cancelled`
- `spec_review -> cancelled`
- `planning -> cancelled`
- `plan_review -> cancelled`
- `building -> cancelled`
- `pr_review -> cancelled`
- `quarantined -> cancelled`
- `halted -> cancelled`

`cancelled` grants nothing (`Cancel` never launches, resumes or merges
anything), and a cancel stops what the request had running. The worker's step
for the request polls its state and, on `cancelled`, ends the step's context
with cause `request.ErrCancelled`: a drafting job or a build's sandbox is killed
at once, the run halts with `HaltReasonCode` `request_cancelled`, its worktree
is rolled back and never kept for a resume, and the step's result is discarded
(`stillInState`). `Request.Save` also refuses to write any other state over a
`cancelled` record on disk (`ErrCancelledOnDisk`), taking the request's lock for
the check and the write when nobody holds it, so a job that finished meanwhile
cannot bring the request back. The one window left is a cancel that lands in the
instant between that check and the rename of a Save made while another process
holds the lock. A pull request already opened stays open. A request halted
on an unconfirmed run can still have a Temporal execution in flight, and
reclaim can still apply its result (including opening a PR); that run belongs to
the halted request's earlier step, not to a step a cancel could stop. Nothing
moves the request out of `cancelled` again. `done` and `closed` are excluded
from `Cancel`'s allowed states: each is a final outcome, not a dead end, and
stays immutable.

### Request lifecycle: a review gate expires

A request waiting at `spec_review`, `plan_review` or `resume_review` is
waiting on a person. With the session-config key
`review_gate_expiry` set (a duration; the default `0` is off), a gate that has
waited that long since the request entered it (`EnteredAt`) is moved to `halted`
with halt kind `gate_expired` and the error `<state> expired: no decision in
<duration> (review_gate_expiry)`, and the operator is notified like any halt (by whoever found the gate expired: the worker, the CLI, or the API's notifier; sent once)
(`internal/request.ExpireIfDue`, run by the worker's review wait, and the same check made inside
`Approve`, `Reject`, `ResumeRequest` and the API's in-place edits of the
spec and tickets, under the request's own lock on the record just read, so a decision and the expiry
cannot interleave; the CLI and the API read `review_gate_expiry` for each decision and refuse the
decision when the session config cannot be read). `pr_review` has no expiry: merging is a
human act and a long-open pull request is normal. Expiry grants nothing: it
approves, redrafts and builds nothing, and it never reaches `cancelled`, which
stays the operator's own decision.

- `spec_review -> halted`
- `plan_review -> halted`
- `resume_review -> halted`

`factoryd retry` / `POST /requests/{id}/retry` on a `gate_expired` halt
(`internal/request.ReopenReview`) returns the request to the review it expired
at, with a fresh `EnteredAt` and reminder state and nothing redrafted; a
`resume_review` needs the lost step it waited on (`Resume`):

- `halted -> spec_review`
- `halted -> plan_review`
- `halted -> resume_review`

### Request lifecycle: a one-ticket plan goes to building

The spec approval stays the human approval of what is built. A request
submitted without a plan runs no planning model job: its planning step writes
one ticket that is the approved spec byte for byte, so the build is given the
spec verbatim and builds it in one session. That plan waits at `plan_review`
for a person only when the request opted in (`factoryd submit -plan-review` or
the session-config key `plan_review`), when the repository's `.factory.yml`
sets `plan_review: true`, or when a person already sent a plan of the request
back; a plan the planning job drafted after a size refusal (below) always
waits there. Any other plan is approved by the factory itself in the planning
step (`internal/request.ApprovePlanByFactory`): it re-verifies every recorded
hash, runs the checks of a person's plan approval (the same
`planApprovalHashes` function: each ticket spec re-validated), pins the
SHA-256 of every ticket file into `ApprovedSHA256`, and moves the request to
`building`, recording the factory in the History entry, keeping the plan's
review notes on the record, and leaving `ApprovedBy` and `ApprovedAt` to a
person. Any refusal of those checks leaves the request unchanged and moves it
to `plan_review`, so a person decides: the factory fails toward the human,
never toward building. Every later step re-verifies the pinned hashes exactly
as after a person's approval, and every gate (verify, policy, review, release)
is unchanged.

A person who sends that one-ticket plan back (`reject -to plan`) starts the
planning model job, given the rejection's reason; its plan waits at
`plan_review`. A `Required-Changed-Files:` or `Required-Content:` header
in a ticket file, drafted or handed over, is ignored: no step reads it and no
note is written for it. No request state lies
between `spec_review` and `planning`: a request record in a state this
release does not define (`oracle_drafting`, `oracle_review`) is refused when
read, with a message naming the state, so such a request is finished or
cancelled before an upgrade.

- `planning -> building`

### Request lifecycle: an oversized or mixed change goes back to planning

A request ticket whose run passes every gate before the code review and
measures over either release hard cap (the paragraph on size after SC-018)
opens no pull request, and the request moves
back to `planning`, given the measurement and the run's diff as fenced data:

- `building -> planning`

A request whose ticket's carved commits (`pr_order`, the paragraph after the
size paragraph) are refused by a gate makes the same move with the same
inputs and the groups.

The move grants nothing. The oversized run is not reviewed or pushed, opens no pull
request and is no ticket's PR-head run, and the tickets it was built from are
replaced by the plan the planning job drafts, which waits at `plan_review` for
a person whatever its number of tickets. Every ticket of that plan is built
from its own base, judged by every gate and measured again, and the hashes a
person approves are pinned as for any plan. A request moves to `planning` for
size or order once: a ticket that measures over a cap or changes files of
more than one `pr_order` group again, or a ticket of a plan handed over
(`submit -plan-dir`) that does, opens no pull request and halts
the request with the measurement, where a person cancels it or sends it back
(`reject -to plan|spec`).

### Request lifecycle: a pull request closed without merging

The factory never merges and never re-opens a pull request a person closed.
When the PR-review poll reads the last ticket's pull request as `CLOSED` with
no merge, `internal/request.Close` ends the request:

- `pr_review -> closed`

`closed` is terminal like `done`: nothing moves a request out of it, and
`Cancel`, `Retry` and `Quarantine`/`Halt` refuse it. Only an explicit
`CLOSED` state reaches it; an empty or unknown pull request state is a
failed read, polled again (`handleClosedOrMergedTicket`,
`internal/requestdriver/pr_review_driver.go`). Another ticket's pull request
still open is left as it is, for a person to merge or close.

An earlier ticket's pull request, which a later ticket's commits sit on, read
as `CLOSED` with no merge parks the request in `resume_review` instead, with
a reason and no cause (`pr_review -> resume_review`, below). So does, before
a later ticket's build starts, any earlier ticket's pull request read as
closed, or open with its branch gone from origin (`building -> resume_review`);
the ticket just before it with an open branch at a commit other than its
current PR-head run's `ResultSHA` leaves the request `building` with no build
started, checked again on the next pass and reminded at the reminder interval
(`internal/requestdriver/stacked.go`). Nothing was lost and nothing kept, so a
resume reruns the parked step, which checks again; a park that interrupted the
resume of a lost build keeps that build's run, and the next resume continues
it after the same precondition checks. A restack (the Forge row) parks the
same way, from `pr_review`, for a commit the factory did not make on the
later ticket's branch, a merge commit or a commit no run of the ticket
judged in its range, a leased push that failed, commits whose cherry-pick
stopped on a conflict (git's `CHERRY_PICK_HEAD`, read by the host: the
cherry-pick is aborted and its scratch worktree and branch removed), or a
replayed commit that is not the same change as its original
(`internal/requestdriver/restack.go`). A conflict is a fact about the
commits, not a lost step: its reason says that a resume replays again,
which succeeds once the branch under the ticket has moved, and that a fix
pushed by hand to the ticket's branch is refused as a human commit.
A required CI check that completes with a failure again on the head its CI
round pushed parks the request the same way, from `pr_review`, with a reason
naming the check and no cause (the paragraph on CI checks after SC-018); a
resume reads the checks again and starts no round for that check.

### Request lifecycle: a lost step waits for a human

A request step (a drafting or planning step, a ticket build, a corrective
or PR-review round) whose worker stopped, or a ticket build or round that
ended with no verdict on the work for another of SC-017's causes, or whose
only failed gates are reviews that gave no verdict, is never
rerun on its own. A ticket whose recorded run halted with such a cause,
and about which no decision of the current `Generation` exists, is moved to
`resume_review` by the step that finds it, also when the request is still
`building`: its kept worktree is not reaped and no build starts. A run that
started and left a finished record is read even when its step's context was
cancelled.
`internal/request.EnterResumeReview` moves the request into `resume_review`,
a human wait with reminders, recording the lost state, the lost run, the
cause and a `Generation`:

- `spec_drafting -> resume_review`
- `planning -> resume_review`
- `building -> resume_review`
- `pr_review -> resume_review`

`factoryd resume` / `POST /requests/{id}/resume` (`internal/request.ResumeRequest`)
records the decision and returns the request to the state of the lost step:

- `resume_review -> spec_drafting`
- `resume_review -> planning`
- `resume_review -> building`
- `resume_review -> pr_review`
- `resume_review -> cancelled`

`cancelled` is `Cancel`, as above. A build resumed with `round` is checked
again before it launches; a failed check returns the request to
`resume_review` (`building -> resume_review`) with the reasons, and only
`-from scratch` or cancel are possible. `Quarantine` and `Halt` still reach
`quarantined` and `halted` from `resume_review`, a non-terminal state.
`retry` refuses a request in `resume_review`.

## Gate-retirement conditions

The pre-merge gates below are scaffolding and may be retired once the
following evidence exists for the target repository. **Human approval of
merge itself is not on this table.** By permanent operator decision, the
factory never retires it, whatever the evidence.

| Gate to retire | Required evidence before removal |
|---|---|
| Human approval of product/spec/architecture/design | Deterministic policy checks, durable inputs/outputs, fail-closed behavior, and an asynchronous notification path. |
| Human approval of a slice | Canonical oracle outside the worker, scope/protected-path checks, rollback pointer, and crash/retry recovery evidence. |
| Credential/network exception | Containment escape matrix and a policy and meter-ledger audit proving only explicitly allowed requests and no credential exposure. |

Until every applicable row is demonstrated, the factory must stop before the
corresponding unattended side effect. A passing unit test or reviewer-agent
verdict alone is not sufficient evidence for retiring a gate.

## Evidence and approval record

For each run, the durable record must identify the project, ticket, workspace,
base/result revisions, state transitions, attempt limits and outcomes, exact
oracle/verify commands and hashes, changed files, policy inputs/outputs,
notifications, overrides, and release/rollback data. Large logs and artifacts
may live on disk, but their path, hash, ownership, and timestamps belong in
the durable evidence store. The target single-machine store is SQLite in WAL
mode. The event log already uses it (`internal/run/events.go`), but run
records are still per-run JSON files (`internal/run/run.go`), so this
remains `partial`.

The machine-checkable test verifies that these sections, invariant IDs, and
state edges cannot be silently removed while the implementation claims to
satisfy this contract.
