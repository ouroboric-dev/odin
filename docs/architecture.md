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
- Walks the graph and runs exec, agent, loop and gate nodes; checkpoints runs and resumes them. A
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
  consumes (spec §6.1, §9.2).
- Counts every session's usage, a failed one's too, in the run and in a daily spend journal outside
  any project, and stops the run at the hard approval point `budget-cap` before a session would start
  over a cap (spec §7.4). Pauses the run when the provider's quota is used up (spec §12).
- Walks the instance tree and moves pointers. It is the only writer of umbrellas, `features.json`,
  the ledger, gate files and reports (spec §3.3).
- Does every git operation, from the host (spec §6.3).
- Writes the hash-chained ledger and serves the `ygg` CLI.
- Holds routing (spec §8) and observability (spec §10) as packages.

## Depends on

- Gungnir's interfaces, Gleipnir (adapters, the sandbox, the check runner) and Jormungandr (the loop).

## Source

The v0 engineering spec, §2.1, §3, §4, §8, §10. When this page and the spec differ, the spec wins.
