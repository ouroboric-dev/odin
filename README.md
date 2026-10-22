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
ygg approve <run-id> <gate> [--note <text>] [--project <dir>] [--mode <mode>]
ygg reject  <run-id> <gate> --reason <text> [--project <dir>] [--mode <mode>]
ygg status  <run-id> [--project <dir>]
ygg grade   <run-id> [--task <id>] [--project <dir>]
ygg deps bump <package> <version> --canary <leaf> [--rollout <leaf>]... [--host] [--project <dir>] [--config <factory.yaml>]
ygg doctor  [--project <dir>] [--config <factory.yaml>] [--root <dir>]
ygg sandbox build (--realm <name|dir>)... | --all [--config <factory.yaml>]
```

Exit codes: 0 done, 1 a node failed, 2 refused, 3 stopped at an approval point, 4 paused (the provider's quota is
used up), 5 done with tasks given up (unattended).

`factory.yaml` must set both budget caps, `per_run` and `per_day`, each with `tokens` and `turns`.
The factory ships no defaults, so a config without them runs nothing.

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
