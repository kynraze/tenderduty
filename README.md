# TenderDuty v2

[![Go Reference](https://pkg.go.dev/badge/github.com/kynraze/tenderduty/v2.svg)](https://pkg.go.dev/github.com/kynraze/tenderduty/v2)
[![Gosec](https://github.com/kynraze/tenderduty/workflows/Gosec/badge.svg)](https://github.com/kynraze/tenderduty/actions?query=workflow%3AGosec)
[![CodeQL](https://github.com/kynraze/tenderduty/workflows/CodeQL/badge.svg)](https://github.com/kynraze/tenderduty/actions?query=workflow%3ACodeQL)

Tenderduty monitors Tendermint validators and alerts when they miss blocks, get jailed, or a node goes down.

This is a fork of [blockpane/tenderduty](https://github.com/blockpane/tenderduty) maintained by [Kynraze](https://github.com/kynraze). Configs from the original still load. This fork includes:

- Dashboard with filters, chain details and themes.
- Fixes for false alerts after restarts, unresolved PagerDuty incidents and alerts lost to state file problems.
- Realio multistaking: several validators on one chain, each with its own nodes.
- Reload with `tenderduty reload` or SIGHUP, keeping unchanged chains and their state.
- Manage chain files with `tenderduty chain add/set/remove/rename/node/list`.
- Daily and weekly reports for Telegram, Discord and Slack.
- One staged signing alert: missing blocks, close to jail, then jailed or out of the active set.
- Governance and upgrade alerts, including Celestia version signalling; optional missed block proposals.
- Celestia bridge: RPC reachability, header sync and sampling, see [bridge.md](docs/bridge.md).
- Celestia Fibre: provider checks, metrics from Fibre itself, endorsements and a dashboard tab, see [fibre.md](docs/fibre.md).
- Docker image at `ghcr.io/kynraze/tenderduty`.

![dashboard screenshot](docs/dash.png)

## Quick start

Docker:

```
mkdir tenderduty && cd tenderduty
docker run --rm ghcr.io/kynraze/tenderduty:latest -example-config >config.yml
mkdir chains.d && docker run --rm ghcr.io/kynraze/tenderduty:latest -example-chain >chains.d/Osmosis.yml
# edit config.yml for the notification methods, and chains.d/Osmosis.yml for the chain
docker run -d --name tenderduty -p "8888:8888" -p "28686:28686" --restart unless-stopped -v tenderduty-state:/var/lib/tenderduty -v $(pwd)/config.yml:/var/lib/tenderduty/config.yml ghcr.io/kynraze/tenderduty:latest
docker logs -f --tail 20 tenderduty
```

From source (Go 1.27.1 or later):

```shell
git clone --branch main https://github.com/kynraze/tenderduty.git
cd tenderduty
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
cp examples/config.yml config.yml
cp -r examples/chains.d chains.d
# edit config.yml for the notification methods, and chains.d/Osmosis.yml for the chain
./tenderduty -f config.yml
```

See [installation](docs/install.md) for Docker Compose, systemd and dashboard access.

## Configuration

Shared settings go in `config.yml`, with `alert_defaults` for the alerts every chain uses. Each chain goes in its own file in `chains.d`, named after the chain: `chains.d/Osmosis.yml` is "Osmosis", the label ends at the first dot. Start from [examples/](examples), or print one: `tenderduty -example-config`, `-example-chain`, `-example-celestia`. Warnings for unknown settings name the closest known one.

Names are case-insensitive; use the exact name if two existing files differ only in case.
`chain set Celestia Akash public_fallback=yes` edits several files; `chain set --all public_fallback=yes` edits every chain file.
`chain show Akash` lists every setting, its current value and source.
`chain rename akash Akash` moves history when tenderduty is running and reloads; a hand `mv` followed by `tenderduty reload` works too.
`chain node add Akash http://localhost:26657` and `chain node remove Akash http://old-node:26657` edit its nodes; an added node alerts when down only with `--alert yes`.

After editing, run `tenderduty reload` to apply the settings that support reload.

## Options

```
$ tenderduty -h
Tenderduty monitors Tendermint validators and alerts when they miss blocks, get jailed or a node goes down.

Usage:
  tenderduty [flags]
  tenderduty [command]

Available Commands:
  start       Run tenderduty (the same as no command)
  chain       Add, change, remove or list chains
  config      Show or change config.yml
  reload      Reload a running tenderduty
  help        Help about any command

Flags:
  -home string               directory for config.yml, chains.d and the state file, or env TENDERDUTY_HOME
                             (default: the current directory if it has config.yml, else /etc/tenderduty
                             with the state in /var/lib/tenderduty)
  -f string                  config file, or env CONFIG (default "config.yml", or <home>/config.yml)
  -cc string                 chain config directory (default "chains.d", or <home>/chains.d)
  -state string              state file (default ".tenderduty-state.json", or <home>/.tenderduty-state.json)
  -example-config            print an example config.yml and exit
  -example-chain             print an example chains.d/<Chain>.yml and exit
  -example-celestia          print an example chains.d/Celestia.yml with Fibre and exit
  -h, -help                  help for tenderduty

Remote config (see docs/remote.md):
  -encrypt                   encrypt -f to -encrypted-config
  -decrypt                   decrypt -encrypted-config to -f
  -encrypted-config string   encrypted config file (default "config.yml.asc")
  -password string           password for -encrypt/-decrypt, or env PASSWORD, prompts if unset

A relative -f, -cc or -state is relative to the current directory, not to -home.

Use "tenderduty [command] -h" for more information about a command.
```

## Documentation

See the [docs](docs/README.md): installation, [shared settings](docs/config.md), [chain settings](docs/chains.md), PagerDuty, Discord, Telegram, Prometheus, Celestia Fibre, remote config and Akash.

## Contributions

Please open pull requests against the `main` branch of [kynraze/tenderduty](https://github.com/kynraze/tenderduty).

CLI setup: `tenderduty config show [section]`, `tenderduty config set key=value`, `tenderduty chain bridge <Name> <url>`, `tenderduty chain fibre <Name> [--instance name]`.
