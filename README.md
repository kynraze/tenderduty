# TenderDuty v2

[![Go Reference](https://pkg.go.dev/badge/github.com/kynraze/tenderduty/v2.svg)](https://pkg.go.dev/github.com/kynraze/tenderduty/v2)
[![Gosec](https://github.com/kynraze/tenderduty/workflows/Gosec/badge.svg)](https://github.com/kynraze/tenderduty/actions?query=workflow%3AGosec)
[![CodeQL](https://github.com/kynraze/tenderduty/workflows/CodeQL/badge.svg)](https://github.com/kynraze/tenderduty/actions?query=workflow%3ACodeQL)

Tenderduty monitors Tendermint validators and alerts when they miss blocks, get jailed, or a node goes down.

This is a fork of [blockpane/tenderduty](https://github.com/blockpane/tenderduty) maintained by [Kynraze](https://github.com/kynraze). Configs from the original still load. What's different:

- New dashboard with filters, chain details and themes.
- Alerting fixes: no false alerts after restarts, PagerDuty incidents resolve, alerts survive state file problems.
- Realio multistaking: several validators on one chain, each with its own nodes.
- Reload with `tenderduty reload`, SIGHUP or file watching, keeping unchanged chains and their state.
- Manage chain files with `tenderduty chain add/set/remove/list`.
- Daily and weekly reports for Telegram, Discord and Slack.
- Celestia Fibre, signal, governance and upgrade alerts.
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

Shared settings go in `config.yml`, with `alert_defaults` for the alerts every chain uses. Each chain goes in its own file in `chains.d`, named after the chain: `chains.d/Osmosis.yml` is "Osmosis", the label ends at the first dot. Start from [examples/](examples).

Edit, then run `tenderduty reload` to apply without a restart.

## Options

```
$ tenderduty -h
Tenderduty monitors Tendermint validators and alerts when they miss blocks, get jailed or a node goes down.

Usage:
  tenderduty [flags]
  tenderduty [command]

Available Commands:
  chain       Add, change, remove or list chains
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
