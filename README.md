# odin

The orchestrator: the graph engine and the `ygg` CLI, with routing and the ledger as packages.

Part of [Yggdrasil](https://github.com/ouroboric-dev/yggdrasil), the software factory by
[Ouroboric](https://github.com/ouroboric-dev/ouroboric). Work on it from the Yggdrasil umbrella,
which holds every component as a submodule and builds them together through `go.work`.

## Build and test

```bash
go test ./...
go vet ./...
```

## Usage

```text
ygg run     --graph <name|dir> [--project <dir>] [--config <factory.yaml>] [--brief <file>] [--mode <mode>]
ygg resume  <run-id> [--project <dir>] [--mode <mode>]
ygg approve <run-id> <gate> [--decisions <file> | --note <text>] [--project <dir>] [--mode <mode>]
ygg reject  <run-id> <gate> (--decisions <file> | --reason <text>) [--project <dir>] [--mode <mode>]
ygg status  <run-id> [--watch [--interval <duration>]] [--project <dir>]
ygg grade   <run-id> [--task <id>] [--project <dir>]
ygg metrics <run-id|first..last> [--json] [--project <dir>]
ygg metrics --compare <run-id> <run-id> [--json] [--project <dir>]
ygg report  <run-id> [--commit] [--project <dir>]
ygg board   [--project <dir>] | --all [--projects <file>]
ygg bridge  --config <factory.yaml> --graph <name|dir> [--work <dir>] [--mode <mode>]
ygg deps bump <package> <version> --canary <leaf> [--rollout <leaf>]... [--host] [--project <dir>] [--config <factory.yaml>]
ygg doctor  [--project <dir>] [--config <factory.yaml>] [--root <dir>]
ygg sandbox build (--realm <name|dir>)... | --all [--config <factory.yaml>]
```

Exit codes: 0 done, 1 a node failed, 2 refused, 3 stopped at an approval point, 4 paused (the provider's quota is
used up), 5 done with tasks given up (unattended).

`factory.yaml` must set both budget caps, `per_run` and `per_day`, each with `tokens` and `turns`.
The factory ships no defaults, so a config without them runs nothing.
When any role bills a paid API (an OpenCode role), the caps also count dollars, and they count what the API billed. The
cost figure of a Claude Code session is what it would cost on the API; the subscription charges nothing for it, so it does
not count against `usd`. `ygg status` and `ygg metrics` show both.

`ygg metrics` reads a run's ledger, `run.json` and `score.json` and prints the figures of spec §10.2: tokens, turns and
cost (with what an adapter only estimated, and sessions that came back with tokens and no cost), the time the factory
worked with the approval waits and the stopped time taken out, the first-pass rate and the average attempts of the loops,
the reviewer veto rate, findings, interventions, blocked tasks, the hidden score and the escaped defects, and what the
routing indexes scored. A rate over nothing says `n/a`. It checks the ledger's hash chain first and refuses a ledger that
was changed. `first..last` lists every run of the project from one id to the other, with a total line. `--json` prints the
same figures for a tool. It writes nothing.

`ygg metrics --compare <runA> <runB>` prints what differed between two runs of one project (the factory build, the
config hash, the agent and model of each role, the realm versions, the mode, the graph) and the deltas, B minus A.
Runs of another brief, task version, suite or mode are marked as not comparable, with the reason, and the deltas are
still printed. A comparable pair names the metrics that improved and the ones that got worse, and says whether the
hidden score held: a factory change counts only if it did (design doc §2.3, criterion 7). A pair that is not comparable
gets no verdict: it prints `verdict: not judged: the runs are not comparable` instead.

`ygg report <run-id>` writes `report.md` into the run's folder: the tasks the run gave up first, the metrics, every
intervention with its note, the loops that gave up, and each finding a dispute overruled with both sides. It names the
ledger line it was read from, and the same run always gives the same bytes. Text an agent wrote is quoted, so it cannot
open a heading or end a block. `--commit` also commits a copy as `docs/runs/<run-id>.md`, because a project ignores `.factory/`.

`ygg status <run-id> --watch` prints the status again every `--interval` (2 seconds by default) while a process holds the
run, with a `now:` line that names the node that is running, its attempt and how long it has run, and the cost so far.
It stops when the run stops running, or at once for a run that no process holds. The live view reads a ledger that is
being written and does not check its chain: `ygg metrics` is the evidence.

`ygg board` shows the project's tasks by column, as a terminal board: Ready, In progress, Verifying, Blocked, Needs you
and Done, in the plan's order, each with its leaf and, for a blocked task or one that needs you, the reason. It reads
`.factory/features.json` and the run that file names, and writes nothing. `ygg board --all` shows the board of every project
in the operator's local list, `$XDG_CONFIG_HOME/ouroboric/projects.yaml` (or `~/.config/ouroboric/projects.yaml`, or the file
`--projects` names): `projects:` and a list of absolute paths. A path that is not a project umbrella, or one project whose board cannot be read, refuses the whole list.
`--projects` without `--all` is refused.

`ygg bridge` is the factory's side of the Harbor bridge (`bench/harbor`). The Harbor adapter starts it and sends the brief
on standard input as one JSON line. It makes a scratch project (and names its folder on standard error), runs the graph on the brief, reports the cost
so far (`spend`), copies the committed product to the task environment's working folder through the adapter
(`upload_dir`), asks for a self-check (`selfcheck`) and says `done`. Standard output carries the bridge and nothing else; the run's own output goes to standard
error. A Harbor run has no one to approve a gate: a run that stops is handed over as it is, and the exit code is the
run's. The config's `budget.per_run.usd` must not be above the task's budget when a role bills per token.

`ygg deps bump` runs the package manager and the checks in the sandbox of `factory.yaml`. Without a `sandbox:`
section it refuses (exit 2) unless you pass `--host`, which runs the bump on the host with the full network and
your user id, and records that in the first line of the bump's ledger. With a sandbox section `--host` has no effect.

`--project` names a project umbrella. A project is usually its own tree, its `umbrella.yaml` setting
`root: true`; inside a company umbrella, every umbrella above it lists the one below, up to the one
that sets `root: true`. A run works on its
own branch, `ygg/<run-id>`, in each leaf its graph names, and commits a checkpoint after every node.
`--graph` names a graph pack, from `graphs.dir` in `factory.yaml` or as a path; the first run pins it in
`umbrella.yaml`, and each leaf's realm comes from its own pin. A leaf the project lacks is created from the
template of its realm. `ygg doctor --root <dir>` checks a tree without a project. With a `sandbox:` section it
also stops the containers and removes the empty pods that a killed `ygg` left, and counts them.

## Docs

- Architecture: [docs/architecture.md](docs/architecture.md)
- Engineering spec: <https://github.com/ouroboric-dev/yggdrasil/blob/main/docs/2-engineering/specs/2026-10-11-yggdrasil-v0-spec.md>

## Contributing

Run `uvx pre-commit install` once after cloning, so `gitleaks` checks each commit for secrets.

## Licence

Code is AGPL-3.0-only. `docs/`, this README and `AGENTS.md` are CC-BY-4.0. See `REUSE.toml`.
