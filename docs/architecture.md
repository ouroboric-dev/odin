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
- Runs the stages before the code: a scratch agent works in a workspace of its own (an `out/` folder for its
  drafts, and the files of its inputs over the bound); `publish` commits its typed output into the project, after
  checking it holds no secret: the output's JSON, the bytes Odin kept, beside its view, the Markdown Odin renders
  from it (`internal/views`, change typed-boundaries: nothing reads a view back). The paths are the ones the
  prompts gave: under the node's `to`, `1-product/design/<date>-<slug>`, `2-engineering/specs/<date>-<slug>`,
  `2-engineering/plans/<date>-<slug>/{plan.json,plan.md,tasks.md}` and
  `2-engineering/reviews/<date>-<slug>-reuse-review` (the slug of the plan it reviews), with the run's `date`. A
  spec's publish also writes `leaves.yaml` from the spec's typed leaves. Each view starts with front matter Odin
  writes (`title`, `status: draft`, `date`, `related`, which links the views of the outputs the node read), each
  value a quoted scalar. A blocked output is not published: the publish stops the run at
  `external-input` for a person, before any scan, with the node and its reason in the gate file, and approving runs the
  source again with what the person gave as its `rejection` (with nothing given, that a person read the reason and
  sent it back; ruling R43). When the secret scan finds something, nothing is published
  and the run stops at the hard approval point; approving runs the source node again with the findings (the rule
  and the place, never the text), and then what the person gave, as its `rejection`, and its new output is kept,
  published and scanned again. A
  publish node whose source declares no output, has no view, or takes no `rejection` is refused when the run
  starts. `tasks` reads the plan node's kept output (`ygg.plan/v1`, as Odin validated it; nothing reads a `tasks.md`),
  applies the Definition of Ready and writes `features.json` with every field of each task. A task that is not ready,
  such as one whose text or contract is over its bound or whose contract is not OpenAPI, goes back to the planner as
  its `refusal`; a blocked plan stops the run at `external-input` for a person, as at a publish, and what they give
  at approval goes to the planner as its `rejection` (a second line, for a graph that reads the plan before publishing
  it; R44). The `task` input of every session of a task holds its title, layer, criteria, named
  test and dependencies, then its text under "What the plan says" and its contract under "The contract", in a
  `yaml` fence, whole: Odin never cuts a task's words (B-176). `init` creates the leaves the spec stage named from
  their realms' templates and records the pins. Scratch sessions (the nodes marked `scratch: true`: the design, spec,
  plan and reuse-review stages and the conformance check) run in a Gleipnir sandbox on the factory layer,
  the stack every session shares, never on a realm's stack; the model APIs of their adapters are their
  only egress. A loop's implementers and reviewers are no scratch sessions: they run in the leaf's
  sandbox, on the realm's stack.
- Repeats a task shape for every ready task (`foreach`), in the leaf and the realm of the task. A
  merge gate or pointer move that refuses the work's checks sends it back into the loop that made it
  (`back_to`, a few times). An unattended run gives up a task whose loop gave up, keeps its work on a
  ref, and goes on with the tasks that do not depend on it.
- Runs a loop on typed answers and typed checks (change typed-boundaries). `loopSteps.Implement` and
  `loopSteps.Review` read the implementer's `ygg.implement/v1` answer and the reviewer's `ygg.review/v1` report
  through the output channel, as every node does: asked for once more when it does not validate, and kept in
  `outputs/<session>.json`. The ladder's checks are typed (`checks.Spec.Typed`, ruling R28) in a sandbox; on the
  host a command takes no environment, so there a check writes no typed result. A failing check gives one
  finding per failure its `ygg.check/v1` result lists, with the test's id and place, or the one finding
  `check-<name>-result` when it lists none, is missing or contradicts the exit code. How the checks last ended
  (each check's pass, the tests that failed and the tests that passed) is kept in the loop's record in
  `run.json` (`LoopRecord.Checks`), and every session after them reads it as `check_results`, the same after a
  resume. Exec nodes, generators, host runs and the deps bump run their commands untyped.
- Settles a disputed finding with a check (change disputed-findings, ruling R23). Jormungandr's loop settles
  each dispute on this attempt's check outcomes: it holds only when the named check ran in this attempt and its
  typed result lists the test among the tests that passed, and not among those that failed. `Record` writes
  `finding_overruled` for a finding that was overruled and, for a dispute that could not be used, a
  `dispute_ignored` line. A dispute no check of the loop can settle stops the run at escalation with the reason
  `finding-disputed` (exit 3, like `attempts-exhausted`); the gate file shows both sides and `ygg approve`
  accepts the implementer's side. `ygg status` lists every overruled finding with the reviewer's claim, the
  implementer's evidence and the check run.
- Stops for a person when a loop's agent says it could not do its work. An implementer that answers `blocked`
  ends its attempt before any check or reviewer, and the loop escalates as `implementer-blocked` with the
  reason; a reviewer that answers `blocked` twice in a row escalates as `reviewer-blocked`. Each reason has its
  own words in the gate file. A rejection gives the loop its attempts again, with the reason as the implementer's
  `rejection`; for a blocked reviewer or one whose answer can't be used twice (`reviewer-unreadable`), the reason is
  that reviewer's `rejection`, and the checks and the reviewer run again on the same work. A loop and each of its
  reviewers must declare `rejection`. An approval accepts the work as it is; `reviewer-unreadable` takes a rejection
  only.
- Puts the task's diff in every task reviewer's prompt (change review-diff). A read-only reviewer has no shell, so
  it cannot run `git diff`. Odin records the leaf's commit in the loop's record (`LoopRecord.Base`) when the loop
  starts, keeps it across resumes (a state without it records the leaf's commit when the loop next runs), and
  a reviewer that declares `change` gets it as that input: the files with their line counts, then the
  patch from the base to the leaf's commit. Host git computes it with hooks, external diff drivers and text
  conversion off, so nothing in the leaf runs. The patch is filled greedily up to `maxChangeBytes` (200 KB): a file
  that does not fit is left out whole and named, and later files that fit are shown. A binary file (a NUL byte) is
  named, not shown. A diff that cannot be computed gives one line and a `loop_attempt` line `change not computed:`;
  the review goes on. Scratch reviewers outside the loop do not get it.
- Takes a person's gate decisions as data (spec §7.2, change typed-boundaries, `internal/engine/decisions.go`).
  `ygg approve RUN GATE --decisions FILE` and `ygg reject RUN GATE --decisions FILE` read a `ygg.decisions/v1` file
  (a regular file, never a symbolic link, at most 64 KiB, no key twice in one object): decisions under ids the operator chooses and keeps across
  runs, answers to this run's design's open questions by their ids, and for a rejection the reason, which it needs
  and an approval may not hold. `--note TEXT` is shorthand for a file with one decision under the next free id, `note-<gate>`, then
  `note-<gate>-2` and on (ruling R47, so a gate met again can take a new note), and
  `--reason TEXT` for a file with only the reason; each is one line (the rule of `ygg-proof-run.sh`), and a file
  with a shorthand is refused. `Approve` and `Reject` check what is given before anything is recorded: the contract
  (unique decision ids, a note of at most 4 KiB); each answer names an open question of a design the run kept,
  once (ruling R45); and a decision id already kept, at any gate, is given with its text or not at all ("decision X
  was already given as …; give a new id", ruling R46(3)). The `gate_approved` or `gate_rejected` event carries the
  file as one line of JSON in its note; `ygg report` shows it as its ids and its reason's first line.
  At a gate node the decisions and answers are the product owner's, kept in `State.Decisions` as
  `{gate, decisions}` in the order given; a gate met again (a `back_to` return past it, a rejection there and then an
  approval) adds what is new to what it was given, an answer given again replacing the earlier one, and nothing given
  keeps it. At any other stop they stay on the event. A session that declares `decisions` gets every gate's decisions and then
  their answers as the lists of `ygg.decisions/v1`, each with its id and its gate, an answer with its question.
  Every stage that runs again after a person decided (a gate's or a loop's rejection, the secret-scan stop, a
  blocked output) gets as its `rejection` what they gave, rendered by one function (`rejectionText`): the reason,
  each decision with its id (under "The product owner's decisions" at a gate node, "What the person said" at any
  other stop), each answer with its question, every field made safe. A rejection that goes back past
  gates drops their decisions; a resume keeps them from `run.json`.
- Checks that the spec cites every owner decision (design §4, `internal/engine/ownercheck.go`). Right after an
  agent node keeps a `ygg.spec/v1` output, before the conformance review, the ids of every decision in
  `State.Decisions` must each be cited by one of the spec's decisions or deviations. A spec that misses some goes
  back to its own node once, with "the spec does not cite the product owner's decisions IDS: cite each in a decision
  or a deviation" as its `refusal` and a `node_returned` event (`State.Returns` counts it; a rejection that runs the
  spec again starts it over). A node's `rejection` and `refusal` are dropped only once its output passes the check,
  so the spec run again still has the person's reason. A second miss stops the next gate in every mode, whose file lists the ids under "Owner
  decisions the spec does not cite"; with no gate after the spec, the node fails. A blocked spec is left to its
  publish, and an open question with no answer stays allowed. A spec node that does not declare `decisions` and
  `refusal` is refused when the run starts.
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
- Reads every agent session's output through one channel (change typed-boundaries): the document the
  session handed to the `submit` tool, or else its whole final answer as exactly one JSON document, decoded
  against the node's contract. An output that does not validate is asked for once more, in a new session told
  each error; a second one fails the node. Nothing is read out of prose. The output that validated is kept in
  `outputs/<node>.json` of the run folder, with its hash in `run.json`, and a later stage reads it from there.
  A run started before this change keeps no outputs, and a resume refuses it.
- Reads the design-conformance verdict (the `ygg.conformance/v1` output of a scratch reviewer that reviews
  another scratch agent's output, which it reads as its input) and gives it to the spec gate: a deviation
  stops the gate in every mode, with the proposed amendment in the gate file, and so does a verdict that says
  the reviewer is blocked, with its reason (spec §4.2, §4.6). A gate file also shows what a person decides on
  there: the open questions of a design, and the verdict and the items of a reuse review, of the nodes since
  the gate before.
- Builds every session's prompt from the inputs its node declares (change typed-boundaries, `promptFor` in
  `internal/engine/prompt.go`): the role's prompt file and its skills, as they are; for a scratch node, a note on
  its workspace; one section `## Input: <name>` for each input, in the declared order; then how to hand back the
  answer (`jormungandr.AnswerInstructions`, or a reviewer's `ReviewRequest`), through `submit` when the role's
  adapter has it. The brief passes as it is, but for a code fence it leaves open, which is closed. Every other input is rendered from its typed value (`inputs.go`): an
  earlier node's kept output field by field in its schema's order, under `### <field>`, a list of strings as
  bullets and anything nested as a JSON block; the task with its text and its contract whole, the contract in a
  fence no run of backticks in it can close; the change, the open findings and the check results from the loop;
  the date from Odin's clock (`Setup.Clock`). No line of a field's text can start a heading, front matter or an
  HTML block: one that starts with `#`, `---` or `<`, after any blockquote or list markers, or that underlines, is
  indented four spaces,
  and a fence it leaves open (read as CommonMark reads one) is closed. Prompts, views and gate files share this
  one rule (`internal/mdsafe`).
  A declared input with no value fails the node before any session; `decisions`, `rejection`, `refusal`,
  `open_findings` and `check_results` may be empty, and say "None.". An input over 96 KiB goes to a file the
  section names: `inputs/<name>.md` in a scratch workspace, and in a repo `.ygg-inputs/<name>.md`, which the repo's
  git ignores by a rule in its git dir (`git.Repo.Exclude`) and which Odin removes after the session, before the
  checkpoint, and on every other way out (ruling R36). A loop with no base commit gives its reviewers a change that
  says so. A scratch workspace holds `out/` and those files, and nothing of the project. A loop's
  sessions declare its own sources (`change`, `open_findings`, `check_results`) wherever the loop is (ruling R37),
  and get exactly what they declare. A foreach body's input that names a body node names the task's copy of it.
- Checks the graphs with `ygg doctor`: each pack in `graphs.dir` must load, hold a graph and have the
  name of its folder, or `--graph <name>` could not find it. When `graphs.dir` is `realms.dir`, a pack with
  no graph is a realm and is not an error. It warns when `factory.yaml` has no `graphs`
  section, since a project can then not pin a graph.
- Counts every session's usage, a failed one's too, in the run and in a daily spend journal outside
  any project, and stops the run at the hard approval point `budget-cap` before a session would start
  over a cap (spec §7.4). Pauses the run when the provider's quota is used up (spec §12).
- Grades a build (spec §11, change benchmark-split). The `grade` node is the last node of a graph but the
  retrospective. With no `grader` section in `factory.yaml` it is skipped: the node finishes with the result
  "graded: skipped, no grader in factory.yaml", the run goes on, and the record holds no grade (ruling R63).
  Otherwise it stops every sandbox first, exports the committed files of the project as `/app` (with, for each leaf
  that holds an executable `release.sh`, its release at `.ygg/release/<leaf path>/`, see below), and runs the
  hidden suite of the task in a container of Gleipnir's `Grade`: no network, `/app` read and write, `/tests` read
  only, one empty folder for the suite's own logs, and no other mount. It checks the task first (`task.toml`, the
  hash of `instruction.md`, the suite version, the retired mark, the suite's `manifest.json`) and refuses with
  `grade_refused` and a reason, before it builds anything. It gives each attempt a random key (16 bytes, hex),
  which the container gets as `YGG_SCORE_KEY`, and reads the scores from the container's stdout: the one line
  `ygg-results/v1 <key> <report>`, the Playwright JSON report on a single line. The app shares the container and
  can write to the logs folder, so Odin never opens it. A line without the key is ignored; no line with the key, or
  two or more, or a line that is not a report, is a failed grade (`GradeFailed`: no `score.json`, a resume grades
  again), never a score of 0. The key is in no event and no state; it can stay in `grade.log`, which is `0600` and
  never for an agent. It joins the manifest and the report and writes `score.json` to the run folder and nowhere
  else. Fail-to-pass scenarios decide the reward, by category and weight. Pass-to-pass scenarios are reported and
  never change it. An agent never sees the suite folder, `score.json` or the container's output. `ygg grade
  <run-id>` grades a done run that has no grade node or whose grade was skipped, or grades again, and needs the
  `grader` section; `--task` names the task when the brief of the run is not the `instruction.md` of a task.
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
- Serves the local models' list to the scripts (local-models spec §3.2 to §3.4): `ygg models preset FILE` prints
  llama-server's preset INI, `ygg models server FILE` prints NAME, NETWORK, PORT, HOST_PORT, WEIGHTS and STATE as
  `KEY=value` lines (`schema.ParseModels` checked every value, and the command refuses a WEIGHTS or STATE path
  that is not plain, `^/[A-Za-z0-9._/@+-]*$`, so a shell reads the lines unquoted; a symlink serves a path with
  spaces); with `--config FACTORY` it adds `ENGINE_WEIGHTS` and `ENGINE_STATE`, those two paths mapped through the
  factory's `sandbox.mounts` with `sandbox.Config.MapPath` (the Podman machine sees host files only through them),
  and refuses, with nothing on stdout, a factory with no sandbox section, a path no mount covers, or a mapped path
  that is not plain; `models.Ready` still writes `<state>/allowed` on the host path), `ygg models sources FILE` prints, for each model in sorted name order and each file of its `download` in the list's
  order, the line `<model> <repo> <revision or main> <path>` (a model with no `download` prints nothing; a bad list is
  refused with nothing on stdout), for the setup script to fetch; `ygg models image [--cuda-arch N]`
  prints the tag of the model server's image without building it (the tag `build` with the same value prints: another
  architecture gives another tag), and `ygg models build [--cuda-arch N]` builds the
  image (`Driver.BuildModels`) and prints the tag. `ygg doctor` checks each local role (`checkModels`): the list
  not holding the model is an error; a server that does not answer at the host URL (it names
  `scripts/ygg-models.sh start`), or a model network that `podman network inspect` does not call internal, is a
  warning, and so are roles that name more than one local model (a run may use only one). A config with no local role
  adds no line.
- Runs one local model per run (local-models spec §3.9). `Prepare` reads the model list only when a graph role is
  local, and refuses a graph whose local roles name more than one model, or one the list lacks. `Start` and a resume
  (`Open`, `OpenWith`: resume, approve, reject) call `Ready` on the model server before any sandbox starts, so no
  session's time limit pays for the load; a done run and the bridge's export (`OpenForExport`) run no session and never
  call it. The leaf and scratch sandboxes join the model network; the snapshot, grader and deps sandboxes never do.
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

The operator's judgements (`ygg score`, `judgements.json`, `ygg.judgement/v1`) are a score from 0 to 10 with what went well and what went wrong, of the run or of one task. A new one replaces the last of the same run or task in the file, the ledger keeps each as a `judged` line, and the record carries the file's list.

The experiment record (`ygg record`, `record.json`, `ygg.run-record/v1`) is the same figures with the run's setup, each
task's size and findings and the harness's health; `ygg metrics --json` prints it, validated. The setup's hashes are
the ones `run.json` keeps: the graph's and the realms', and each prompt's and skill's, which the run takes from the
bytes it reads (`prompt_sha256`, `skill_sha256`), so a file changed later does not change a record; the record never
reads the prompts again. A run uses one set of instructions: a session whose prompt or skill no longer hashes as the
run kept fails its node before it starts ("the prompt X changed during the run …; start a new run"). The lines of a task come from `git diff --numstat` in its
leaf, from its loop's base to its merge's commit, with nothing checked out; blocked answers and notes come from the
kept outputs, hash-checked and validated, never from prose. What it cannot read is a warning, not an error. A run
started before `run.json` kept the hashes and resumed by a build that keeps them has the hashes of what it read after
the resume only: the record names each prompt of the graph's nodes and each skill with none.
The engine writes `record.json` whenever a run ends, done, failed or stopped, and before a node that reads the `record`
input. A record that cannot be built or written is a warning of the outcome that names `ygg record`, never a failure,
and the outcome gives it once at most: the record written at the end stands for any written before.

The retrospective is an ordinary agent node after the grade: a scratch node that declares the input `record` and returns
`ygg.retro/v1`, with its own role, `retrospective`, in `factory.yaml`. A run that fails before it, the grade included,
gets none, and a retrospective that fails fails the run with the grade kept in the record. Its record input is the
record without the judgements and without its own answer, and of the hidden suite the score's totals only (no
category, no test); it is built apart from the whole record, so a record.json that cannot be written does not stop it.
Its workspace holds `ledger.jsonl`, a copy of the ledger with no `judged` line and whose grade lines lose their result
and note, and `transcripts/`, a copy of the run's transcripts in name order of at most 2,000 files and 64 MiB: a file
larger than the bytes left is skipped, and its prompt says how many were skipped or left out. A `transcripts` folder
that is a link is refused. Its kept answer is the record's `retro`.

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
