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
ygg run     --realm <dir> [--project <dir>] [--config <factory.yaml>] [--brief <file>] [--mode <mode>]
ygg resume  <run-id> [--project <dir>] [--mode <mode>]
ygg approve <run-id> <gate> [--note <text>] [--project <dir>] [--mode <mode>]
ygg reject  <run-id> <gate> --reason <text> [--project <dir>] [--mode <mode>]
ygg status  <run-id> [--project <dir>]
```

Exit codes: 0 done, 1 a node failed, 2 refused, 3 stopped at an approval point.

`factory.yaml` must set both budget caps, `per_run` and `per_day`, each with `tokens` and `turns`.
The factory ships no defaults, so a config without them runs nothing.

## Docs

- Architecture: [docs/architecture.md](docs/architecture.md)
- Engineering spec: https://github.com/ouroboric-dev/yggdrasil/blob/main/docs/2-engineering/specs/2026-10-11-yggdrasil-v0-spec.md

## Contributing

Run `uvx pre-commit install` once after cloning, so `gitleaks` checks each commit for secrets.

## Licence

Code is AGPL-3.0-only. `docs/`, this README and `AGENTS.md` are CC-BY-4.0. See `REUSE.toml`.
