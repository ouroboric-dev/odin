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

- Loads realm graphs and `factory.yaml`.
- Walks the graph and runs exec, agent, loop, gate and grade nodes; checkpoints runs and resumes them. A
  loop node is Jormungandr's loop with Odin's steps: sessions, checks, ledger lines and `run.json`.
- Loads realm leaves (change realm-leaf-naming): `--realm` names the lead realm, found in the realm
  store of `factory.yaml` or given as a path; each leaf runs with the realm the project pins for it;
  each realm loads once, at the pinned version, and two pins that disagree stop the run before any
  sandbox comes up. `run.json` records every realm with a hash of what a run could read, and a resume
  refuses a realm that changed. The first run of a project pins its lead realm.
- Runs the stages before the code: a scratch agent works in a workspace of its own (the brief, copies
  of what it reads, an `out/` folder); `publish` copies its documents into the project and commits them,
  after checking they are documents and hold no secret; `tasks` reads `tasks.md`, applies the Definition
  of Ready and writes `features.json`; `init` creates the leaves the spec stage named from their realms'
  templates and records the pins.
- Repeats a task shape for every ready task (`foreach`), in the leaf and the realm of the task. A
  merge gate or pointer move that refuses the work's checks sends it back into the loop that made it
  (`back_to`, a few times). An unattended run gives up a task whose loop gave up, keeps its work on a
  ref, and goes on with the tasks that do not depend on it.
- Starts each leaf's sandbox on its realm's stack, with the services of its realm and of the leaves it
  consumes (spec §6.1, §9.2). A realm's stack is an image pinned by digest or one that `ygg sandbox build`
  builds from the realm's own build context (change realm-content); the tag is a hash of the context.
- Runs a pointer move's acceptance checks, when the run has a sandbox, in a sandbox that holds a snapshot
  of the project: the committed files of the umbrella and of each leaf, at the commits they are on, in a
  plain folder with no git directory, on the lead realm's acceptance stack and with the services of every
  leaf's realm. What a leaf's working copy holds beyond its commits never reaches it.
- Reads the design-conformance verdict (`out/conformance.json` of a scratch reviewer that reviews another
  scratch agent's output, which it finds as `reviewed/`) and gives it to the spec gate: a deviation stops the
  gate in every mode, with the proposed amendment in the gate file; a verdict that cannot be read is asked
  for once more and then stops the run, never a pass (spec §4.2, §4.6).
- Counts every session's usage, a failed one's too, in the run and in a daily spend journal outside
  any project, and stops the run at the hard approval point `budget-cap` before a session would start
  over a cap (spec §7.4). Pauses the run when the provider's quota is used up (spec §12).
- Grades a build (spec §11, change benchmark-split). The `grade` node is the last node of a graph. It stops
  every sandbox first, exports the committed files of the project as `/app`, and runs the hidden suite of the
  task in a container of Gleipnir's `Grade`: no network, `/app` read and write, `/tests` read only, one empty
  folder for the results, and no other mount. It checks the task first (`task.toml`, the hash of
  `instruction.md`, the suite version, the retired mark, the suite's `manifest.json`) and refuses with
  `grade_refused` and a reason, before it builds anything. It joins the manifest and the suite's
  `results.json` and writes `score.json` to the run folder and nowhere else. Fail-to-pass scenarios decide
  the reward, by category and weight. Pass-to-pass scenarios are reported and never change it. An agent
  never sees the suite folder, `score.json` or the container's output. `ygg grade <run-id>` grades a done
  run that has no grade node, or grades again; `--task` names the task when the brief of the run is not the
  `instruction.md` of a task.
- Limits the grading container with `grader.memory` and `grader.cpus`. `grader.memory: none` and
  `grader.cpus: 0` ask for no limit, for an engine that cannot apply one; `ygg doctor` warns when either is
  set, because only the timeout then holds a suite that hangs. `ygg doctor` also finds the grader containers
  a killed `ygg` left (label `ygg.grader`) and removes the ones no live run holds.
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
  working copies.
- Walks the instance tree and moves pointers. It is the only writer of umbrellas, `features.json`,
  the ledger, gate files and reports (spec §3.3).
- Does every git operation, from the host (spec §6.3).
- Writes the hash-chained ledger and serves the `ygg` CLI.
- Holds routing (spec §8) and observability (spec §10) as packages.

## Depends on

- Gungnir's interfaces, Gleipnir (adapters, the sandbox, the check runner) and Jormungandr (the loop).

## Source

The v0 engineering spec, §2.1, §3, §4, §8, §10. When this page and the spec differ, the spec wins.
