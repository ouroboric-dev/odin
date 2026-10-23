---
title: Odin architecture
status: draft
date: 2026-10-12
related: [https://github.com/ouroboric-dev/yggdrasil/blob/main/docs/2-engineering/specs/2026-10-11-yggdrasil-v0-spec.md]
style: reference
---

# Odin architecture

The orchestrator: the graph engine and the `ygg` CLI, with routing and the ledger as packages.

## Responsibilities

- Loads the graph pack and `factory.yaml`.
- Counts the budget caps. The dollar caps count `Usage.BilledUSD`, which the engine sets for a session of a paid adapter
  (`billed`), and not the session's cost figure: a subscription's sessions have a figure and bill nothing.
- Walks the graph and runs exec, agent, loop, gate and grade nodes; checkpoints runs and resumes them. A
  loop node is Jormungandr's loop with Odin's steps: sessions, checks, ledger lines and `run.json`.
- Loads the graph pack and the realm leaves (change realm-leaf-naming, Voluspa): `--graph` names the
  graph pack, found in `graphs.dir` of `factory.yaml` or given as a path. The pack holds the graph and its
  prompts and no recipe. Each leaf runs with the realm the project pins for it in `umbrella.yaml`, and
  the realm of a leaf comes from that pin alone. The first run of a project writes the graph pin, a
  top-level `graph:` and `graph_version:` in `umbrella.yaml`; a later run of another graph or version is
  refused. A project that still pins a realm at the top is refused, with the way to pin a graph. Each
  realm loads once, at the pinned version, and two pins that disagree stop the run before any sandbox
  comes up. `run.json` (schema `ygg.run/v3`) records the graph pack and every realm with a hash of what
  a run could read, and a resume refuses a pack or a realm that changed. A run of schema `ygg.run/v2`,
  which a realm led, is refused by `resume` and `status`; `ygg doctor` reports it as a WARN, and its
  folder can go. A pack or realm that is a subfolder of a repo is hashed from its committed files.
- Loads the project's runtime realm (change gallery-runtime, spec §9.4): a realm with no leaf that names
  services and the needs they provide (`provides`), pinned once per project as `runtime` and
  `runtime_version` in `umbrella.yaml`. The spec stage names it under `runtime` in `docs/leaves.yaml`, and
  `init` loads it and records the pin in the commit that records the first leaf it creates (or in a
  commit of its own when every leaf is there); a later leaves file names the same pin or none. A build
  realm's pack lists the services its leaves need (`needs`: `postgres`, `s3`). A need with no provider, or
  any need in a project with no runtime realm, stops the run before any leaf is created or any leaf
  sandbox starts (`run.Setup.CheckNeeds`, with the message of `services.Provider`, where acceptance is
  checked: at prepare and in `init`); the `tasks` node that reads the leaves file (`ready`) checks it too,
  and fails without going back to the planner. The message names the need, the realm, and `runtime` in `docs/leaves.yaml`.
  A runtime realm's pack may name a product start (`product.start`, a file of the realm). `init` then writes
  it as `start.sh` at the project's root, mode 0755, in a commit of its own, and adds `.data/` to the
  project's `.gitignore`; a `start.sh` that differs is replaced in a new commit, one that matches adds none,
  and a project with no product start keeps its own `start.sh`.
- Runs the stages before the code: a scratch agent works in a workspace of its own (the brief, copies
  of what it reads, an `out/` folder); `publish` copies its documents into the project and commits them,
  after checking they are documents and hold no secret; `tasks` reads `tasks.md`, applies the Definition
  of Ready and writes `features.json`; `init` creates the leaves the spec stage named from their realms'
  templates and records the pins. Scratch sessions (the nodes marked `scratch: true`: the design, spec,
  plan and reuse-review stages and the conformance check) run in a Gleipnir sandbox on the factory layer,
  the stack every session shares, never on a realm's stack; the model APIs of their adapters are their
  only egress. A loop's implementers and reviewers are no scratch sessions: they run in the leaf's
  sandbox, on the realm's stack.
- Repeats a task shape for every ready task (`foreach`), in the leaf and the realm of the task. A
  merge gate or pointer move that refuses the work's checks sends it back into the loop that made it
  (`back_to`, a few times). An unattended run gives up a task whose loop gave up, keeps its work on a
  ref, and goes on with the tasks that do not depend on it.
- Settles a disputed finding with a check (change disputed-findings). `loopSteps.Implement` returns the
  implementer's whole answer; `loopSteps.Dispute` holds a dispute only when the named check ran and passed
  in this attempt and its log (`logs/<node>-<attempt>-<check>.log`, read in blocks, at most 9 MiB) contains
  the test id. `Record` writes `finding_overruled` for a finding that was overruled and, for a dispute that
  could not be used, a `loop_attempt` line whose result begins `dispute ignored:` (the event list is closed).
  A dispute no check of the loop can settle stops the run at escalation with the reason `finding-disputed`
  (exit 3, like `attempts-exhausted`); the gate file shows both sides and `ygg approve` accepts the
  implementer's side. `ygg status` lists every overruled finding with the reviewer's claim, the
  implementer's evidence and the check run.
- Puts the task's diff in every task reviewer's prompt (change review-diff). A read-only reviewer has no shell, so
  it cannot run `git diff`. Odin records the leaf's commit in the loop's record (`LoopRecord.Base`) when the loop
  starts, keeps it across resumes (a state without it records the leaf's commit when the loop next runs), and
  `loopSteps.Review` adds a section "The change" after the task block: the files with their line counts, then the
  patch from the base to the leaf's commit. Host git computes it with hooks, external diff drivers and text
  conversion off, so nothing in the leaf runs. The patch is filled greedily up to `maxChangeBytes` (200 KB): a file
  that does not fit is left out whole and named, and later files that fit are shown. A binary file (a NUL byte) is
  named, not shown. A diff that cannot be computed gives one line and a `loop_attempt` line `change not computed:`;
  the review goes on. Scratch reviewers outside the loop do not get it.
- Gives the note of a gate approval to every later session (spec §7.2, finding F1). `Approve` refuses a note
  with a control character (the rule of `ygg-proof-run.sh`) or a line or paragraph separator (U+2028, U+2029),
  and keeps a note that is not blank in `State.Decisions`, as `{gate, note}` in the order given, only when the
  stopped node is a gate: a note at an escalation, `policy-violation` or `shrink-guard` stays on the
  `gate_approved` event. A second approval of the same gate, after a `back_to` return past it, replaces the
  gate's note; a blank one keeps it, since the earlier decision still holds. `decisionsBlock`
  (`internal/engine/decisions.go`) writes "The product owner's decisions" after the brief in the prompt of an
  agent, a scratch agent, an implementer and each reviewer. A rejection that goes back past gates drops their
  notes; a resume keeps them from `run.json`.
- Starts each leaf's sandbox on its realm's stack, with the services `services.For` gives the leaf (spec
  §6.1, §9.2, §9.4): for its realm and the realm of each leaf it consumes, the runtime realm's provider of
  each need, under the name it has in the runtime realm's pack, and the services an older pack names itself.
  `For` takes the umbrella and a loader of realms and reads no run's state, so a command with no run
  can use it too, as `ygg deps bump` does. The wiring's `needs` edges come from it too. `ygg sandbox build`
  (`--all`, or `--realm` for a realm that names services) pulls the service images of a realm with no stack,
  the runtime realm's. A realm's stack is an image pinned by digest or one that `ygg sandbox build` builds
  from the realm's own build context (change realm-content); the tag is a hash of the context.
  `ygg sandbox build` builds each stack with the factory layer on top of it.
- Runs a pointer move's acceptance checks, when the run has a sandbox, in a sandbox that holds a snapshot
  of the project: the committed files of the umbrella and of each leaf, at the commits they are on, in a
  plain folder with no git directory. Each leaf realm that defines the check runs it, in realm-name
  order, on its acceptance stack and with the services `services.For` gives every leaf of the project, in
  a snapshot of its own at `snapshot/<node>/<realm>`; the first failure is the move's, and a project where
  no realm has the check is refused. What a leaf's working copy holds beyond its commits never reaches it.
- Reads the design-conformance verdict (`out/conformance.json` of a scratch reviewer that reviews another
  scratch agent's output, which it finds as `reviewed/`) and gives it to the spec gate: a deviation stops the
  gate in every mode, with the proposed amendment in the gate file; a verdict that cannot be read is asked
  for once more and then stops the run, never a pass (spec §4.2, §4.6).
- Checks the graphs with `ygg doctor`: each pack in `graphs.dir` must load, hold a graph and have the
  name of its folder, or `--graph <name>` could not find it. When `graphs.dir` is `realms.dir`, a pack with
  no graph is a realm and is not an error. It warns when `factory.yaml` has no `graphs`
  section, since a project can then not pin a graph.
- Counts every session's usage, a failed one's too, in the run and in a daily spend journal outside
  any project, and stops the run at the hard approval point `budget-cap` before a session would start
  over a cap (spec §7.4). Pauses the run when the provider's quota is used up (spec §12).
- Grades a build (spec §11, change benchmark-split). The `grade` node is the last node of a graph. It stops
  every sandbox first, exports the committed files of the project as `/app` (with, for each leaf that holds an
  executable `release.sh`, its release at `.ygg/release/<leaf path>/`, see below), and runs the hidden suite of the
  task in a container of Gleipnir's `Grade`: no network, `/app` read and write, `/tests` read only, one empty
  folder for the suite's own logs, and no other mount. It checks the task first (`task.toml`, the hash of
  `instruction.md`, the suite version, the retired mark, the suite's `manifest.json`) and refuses with
  `grade_refused` and a reason, before it builds anything. It gives each attempt a random key (16 bytes, hex),
  which the container gets as `YGG_SCORE_KEY`, and reads the scores from the container's stdout: the one line
  `ygg-results/v1 <key> <report>`, the Playwright JSON report on a single line. The app shares the container and
  can write to the logs folder, so Odin never opens it. A line without the key is ignored; no line with the
  key, or two or more, or a line that is not a report, is a failed grade (`GradeFailed`: no `score.json`, a
  resume grades again), never a score of 0. The key is in no event and no state; it can stay in `grade.log`,
  which is `0600` and never for an agent. It joins the manifest and the report and writes `score.json` to the
  run folder and nowhere else. Fail-to-pass scenarios decide
  the reward, by category and weight. Pass-to-pass scenarios are reported and never change it. An agent
  never sees the suite folder, `score.json` or the container's output. `ygg grade <run-id>` grades a done
  run that has no grade node, or grades again; `--task` names the task when the brief of the run is not the
  `instruction.md` of a task.
- Adds the release to the export. A `Releaser` builds the release of each leaf with an executable `release.sh`,
  and Odin copies it to `.ygg/release/<leaf path>/` (for example `.ygg/release/repos/api/`) in the export only: a
  release is never committed to the project or a leaf. `ygg grade` and `ygg bridge` build it in the project's
  acceptance sandbox: the acceptance stack of the realm that defines an acceptance check the graph runs
  (midgard-react's, which holds Node and Go), over a snapshot of the exported commits (`snapshot/release/<realm>`
  in the run folder), with the registries of every leaf's realm, no agent, no secret and no service: no build
  needs Postgres or S3, and a sandbox with services did not stop on the real engine. Odin runs each leaf's
  `release.sh` in the leaf with `OUT` set to an empty folder of the snapshot and no other variable, after writing
  the toolchain's versions (`go version`, `node --version`, each when the sandbox has it) to the log, then stops
  the sandbox, whether the release was made or not, so the grader never starts beside it. One sandbox builds
  every leaf's release, so a leaf's `release.sh` can write into another leaf's release folder: a known cost of
  sharing the sandbox, which the bounds and the file-type checks do not catch. The whole release has the
  grader's timeout (`grader.timeout`; 20 minutes with no grader section). Odin copies the release out itself, so
  every file of the export is the host user's, and gives the snapshot's folders back to the host user, so that a
  later export can empty them (B-139). A project with no acceptance realm, or a run with no sandbox, exports no
  release; when a leaf ships a `release.sh` all the same, `ygg grade` and `ygg bridge` print
  `no release: the project has no acceptance realm …` (or `no release: the run has no sandbox …` for a run
  with no sandbox) to standard error, and the grade's `grade_started` event carries it as its note. The copy is
  bounded on its own: 128 MiB a file and 512 MiB in all, with no symlink, device or other special file, and no
  symlink on the path from the release's folder down to a leaf's (`.ygg`, `.ygg/release`, `repos`, …), which
  Odin checks before it makes a leaf's `OUT` and before it copies. A release that fails, times out or breaks a bound fails the export, which is left empty, and so
  the grade (`GradeFailed`). The build's output, and the failure after it, go to `release.log` (`0600`, at most 4
  MiB) beside `grade.log`. A release is kept in the run folder (`release/`) under the commits it was built from
  and the releaser's key (the acceptance realm, its version and hash, and its acceptance image), so a second
  export of the same commits by the same releaser does not build it again.
- Limits the grading container with `grader.memory` and `grader.cpus`. `grader.memory: none` and
  `grader.cpus: 0` ask for no limit, for an engine that cannot apply one; `ygg doctor` warns when either is
  set, because only the timeout then holds a suite that hangs. `ygg doctor` also finds the grader containers
  a killed `ygg` left (label `ygg.grader`) and removes the ones no live run holds.
- Finds, in `ygg doctor`, the empty pods that compose leaves behind when a sandbox goes (named
  `pod_ygg<run id>...`, no label), reports their number and removes the ones no live run or bump holds. A
  pod is tied to its run by its name starting with the run's prefix, so a pod that matches no run of the
  project is left to `ygg doctor` in the project it belongs to. A pod that cannot be removed is a warning.
- Builds the routing indexes (spec §8) at a pointer move whose node lists them as generated files, in the
  project umbrella, with no model call: the wiring index (the block of `REPOS.md` between its two markers)
  and the impact index (`.ygg/impact.json`). Every edge is tagged `extracted` (read from a manifest, an
  import, an OpenAPI file or a lock file) or `inferred` (a guess). The generator reads a leaf with a bound on
  the bytes of a file and the number of entries, and never follows a symlink out of it. The impact index is
  read with a bound of 8 MiB, as a regular file and not a link, and it is written atomically (a temporary
  file, synced, then renamed), so a failed write leaves the old index whole. Each build writes
  `routing_index_built`.
- Guards the generated files with the shrink guard: a pointer move is refused, at the hard approval point
  `shrink-guard`, when a generated file keeps fewer than half of the rows of its last version.
- Measures an index before it ships. A pack's `routing: {threshold, bench}` block makes the indexes ship only
  when they answer at least `threshold` of the questions of the project's bench right
  (`.ygg/routing-bench.yaml` by default); an index under the bar, or a bench that cannot be read, fails the
  node and writes nothing. A pack with no block ships its indexes unmeasured.
- Judges a pointer move that changes a contract (the `pointer-contract` gate). It compares the commit the
  project points at with the leaf's `main`, against the impact index of the last shipped version. A change is
  judged by `extracted` edges or by `oasdiff`. A guess alone never passes: the verdict is `escalate`, and a
  person decides in every mode. A shipped index that is over 8 MiB or cannot be read also gives `escalate`,
  and so does a move that changes too many contract files. The commit it judged is recorded in `run.json`,
  and the pointer move refuses any other commit. The ledger line is `pointer_contract_checked`.
- Bumps a dependency with `ygg deps bump <package> <version> --canary <leaf> [--rollout <leaf>]...`. It
  refuses a leaf whose manifest or lock file has uncommitted changes, so that git is the way back. The
  canary takes the bump first and runs its `deps` and `unit` checks. Each product gets a `deps_bump` line
  with the note `started` before the tool runs, and a second line with the verdict. A product fails when the
  tool or the checks change a file that is not the manifest or its lock file, or when the manifest or the lock
  file differs after the checks from what the `deps` check judged. A product that fails is put back as it was,
  the rollout products after it are never touched, and the verdict is `fail`. A bump has its own folder
  under `.factory/deps/`, takes the runs lock of the project, and leaves the change uncommitted in the
  working copies. It runs in the sandbox of `factory.yaml`. With no `sandbox:` section it refuses (exit 2,
  before the lock) unless `--host` is given; the bump then runs on the host, and the first line of its ledger
  is a `deps_bump` line with no verdict whose note says it ran on the host. With a sandbox section `--host` has no effect. A bump that is
  cancelled, or whose sandbox does not start, ends its ledger with a `fail` line, so that `doctor` and
  `status` do not list it as unfinished.
- Walks the instance tree and moves pointers. It is the only writer of umbrellas, `features.json`,
  the ledger, gate files and reports (spec §3.3).
- Does every git operation, from the host (spec §6.3).
- Writes the hash-chained ledger and serves the `ygg` CLI.
- Holds routing (spec §8) and observability (spec §10) as packages.

## Metrics

`ygg metrics` (package `internal/metrics`) reads a run's folder and writes nothing. It verifies the ledger's hash chain
against the anchor in `run.json` and refuses a ledger that does not verify. It reads `run.json` for the few fields it
needs and ignores the rest, so a run written by another build of ygg stays readable. Session lines add up to the usage
(the run's own totals repeat them and are not counted); the sum is checked against `run.json` and a difference is a
warning. The time between a `gate_pending` and its decision is an approval wait; the time from a failure, a pause or a
killed process to the next `run_resumed` is stopped time; neither is the factory's. Grading after `run_finished` is
grade time. A finding line says whether a check or a reviewer raised it (`ygg.finding.origin`), a dispute that could not
be used is a `dispute_ignored` line, and every run records the build of ygg that wrote it (`ygg.factory.version`).

`ygg metrics --compare <runA> <runB>` lays two runs side by side: what differed in the setup, the metrics with the delta
B minus A, and a verdict for success criterion 7 (the metrics that improved, the ones that got worse, and whether the hidden
score held). Runs of another brief, task version, suite or mode are marked not comparable, with the reason, because
their scores are not on one scale. A pair that is not comparable gets no verdict, only
`verdict: not judged: the runs are not comparable`. Metrics with no direction, such as the reviewer veto rate, are shown
and never judged.

`ygg report` (package `internal/report`) renders the run report of spec §10.3 as Markdown from the same checked figures,
the interventions with their notes, and the loops' records in `run.json`. With `--commit` a copy is committed as
`docs/runs/<run-id>.md`, because a project ignores `.factory/`.

`ygg board` (package `internal/tracker`) is the local tracker of spec §10.4: it projects `.factory/features.json` and the
run it names into the `Board` of Gungnir's `Tracker` interface. A task that is doing is in progress while its implementer
works, verifying once the checks and reviewers judge it or its loop has passed, and in needs you when the run stopped at
one of its gates, is paused, failed, or ended with the task still doing. The operator's list of projects for `ygg board --all` is local configuration (`projects.yaml` in the config folder), never part of a tree.

`ygg status --watch` is the live view of spec §10.3: it prints the status block again until the run stops running, with the
node that runs now (`metrics.Current`, the end of the ledger, read while another process writes it).

`ygg bridge` (packages `internal/bridge` and `internal/cli`) is Yggdrasil's side of the Harbor bridge: the start line,
then `spend`, `upload_dir`, `selfcheck` and `done` requests, one at a time, each answered with its id. A reply for another
id, a refusal and a line over 1 MiB are errors. The run, its checks and its sandboxes are the same as for `ygg run`; the
Harbor verifier, not Odin's grader, scores the product.

## Depends on

- Gungnir's interfaces, Gleipnir (adapters, the sandbox, the check runner) and Jormungandr (the loop).

## Source

The v0 engineering spec, §2.1, §3, §4, §8, §10. When this page and the spec differ, the spec wins.
