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
