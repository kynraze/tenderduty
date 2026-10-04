# Settings for config.yml

`config.yml` holds the settings every chain shares: the dashboard, alert destinations, quiet hours and `alert_defaults`. Each chain goes in its own file in `chains.d`, see [chains.md](chains.md). Examples are in [examples/](../examples).

**ProTip:** With only the binary or the docker image, `-example-config` prints [examples/config.yml](../examples/config.yml) and `-example-chain` prints [examples/chains.d/Osmosis.yml](../examples/chains.d/Osmosis.yml):

```
$ tenderduty -example-config > config.yml
$ mkdir chains.d && tenderduty -example-chain > chains.d/Osmosis.yml
```

```
$ docker run --rm ghcr.io/kynraze/tenderduty:latest -example-config >config.yml
$ mkdir chains.d && docker run --rm ghcr.io/kynraze/tenderduty:latest -example-chain >chains.d/Osmosis.yml
```

* [Shared alert settings](#shared-alert-settings)
* [Reload](#reload)
* [Chain commands](#chain-commands)
* [General Settings](#general-settings)
* [Alert Destinations](#alert-destinations)
* [PagerDuty Settings](#pagerduty-settings)
* [Discord Settings](#discord-settings)
* [Telegram Settings](#telegram-settings)
* [Slack Settings](#slack-settings)
* [Quiet Hours](#quiet-hours)
* [Health Check Settings](#health-check-settings)
* [Fibre Metrics](#fibre-metrics)
* [Reports](#reports)
* [Settings for a chain](chains.md)

A few notes on how Go handles YAML:

* Booleans can be specified with either true/false or yes/no
* If a setting is omitted it will default to an empty string for strings, zero for numbers, false for booleans, and nil for arrays and structures.

Unknown settings, missing credentials and invalid RPC URLs are logged as warnings.

## Shared alert settings

Set `alert_defaults` once for every chain. A chain's own `alerts` only needs what differs.

```yaml
# config.yml
alert_defaults:
  consecutive_enabled: yes
  consecutive_missed: 5
```

```yaml
# chains.d/Osmosis.yml
alerts:
  consecutive_missed: 3
```

Several validators on one chain, like Realio multistaking: see [chains.md](chains.md#several-validators-on-one-chain).

## Reload

After editing `config.yml` or `chains.d`, reload a running process:

```shell
tenderduty reload
# Custom state file:
tenderduty reload -state /var/lib/tenderduty/state.json
# systemd:
sudo systemctl reload tenderduty
# Docker (either command):
docker exec tenderduty tenderduty reload
docker kill -s HUP tenderduty
```

The CLI reads `<state file>.pid`, so use the same state path and working directory as the running process. Reload needs Linux, macOS or Docker. `kill -HUP <pid>` also works.

- Applied at once: alert thresholds, destinations, credentials, mentions, `min_severity`, `extra_info`, quiet hours, `alert_defaults`, Fibre alert settings, `fibre_metrics.token`, Telegram commands, reports, `group_node_alerts`, `group_wait_seconds`, `nodes[].server`.
- Needs a restart: dashboard and Prometheus enable/port, `hide_logs`, healthcheck, `fibre_metrics` listen/TLS.
- Unchanged chains keep their state and alarms. Added chains start, removed chains stop and their alarms resolve as "No longer watched". A changed chain id, validator or nodes restarts that chain only.
- A failed load leaves the old configuration running, logs the problems and sends a notice to the enabled Telegram, Discord and Slack destinations.
- A reload that drops `fibre_metrics.token` is refused while the running listener is open to the network; restart tenderduty to apply it.

## Chain commands

`tenderduty chain add`, `set`, `remove` and `list` manage chain files. See [chains.md](chains.md#chain-commands). The old `-add-chain` flag still works and prints a deprecation note.

## General Settings

| Config Setting | Description |
| --- | --- |
| `enable_dashboard` | Enable the dashboard. |
| `listen_port` | TCP port of the dashboard. |
| `hide_logs` | Disable the log feed and hide most node details, for a public dashboard. Also hides Fibre failure reasons, identity errors, version and resource figures. Not fully vetted against leaking node names. Needs a restart. |
| `node_down_alert_minutes` | Minutes to wait before alerting that a node is down. |
| `node_down_alert_severity` | Severity of node down alerts: info, warning, error or critical. |
| `group_node_alerts` | One Telegram, Discord and Slack message for nodes of one server that go down together. Default no. PagerDuty still gets one incident per node. |
| `group_wait_seconds` | Seconds a node down alert waits for others of its server. Default 60, minimum 10. |
| `prometheus_enabled` | Enable the prometheus exporter, see [prometheus.md](prometheus.md). |
| `prometheus_listen_port` | Port of the exporter. |

## Alert Destinations

Enabling a destination in `config.yml` is enough, every chain uses it. A chain only says something to leave it out:

```yaml
# chains.d/Osmosis.yml
alerts:
  telegram:
    enabled: no      # every other chain still gets telegram alerts
```

`enabled: no` in `config.yml` wins: no chain gets that destination. The older `pagerduty_alerts`, `discord_alerts` and `telegram_alerts` under a chain's `alerts` are deprecated; they still turn the destination on.

| Config Setting | Description |
| --- | --- |
| `*.min_severity` | On every destination, also per chain under `alerts`: only send this severity and up (info, warning, error, critical). Empty sends all. A recovery goes where its alert went. |
| `*.mentions` | Who to ping on every alert (discord, telegram, slack). Discord only pings `<@user-id>`, `<@&role-id>`, `@here` and `@everyone`, and only the ones listed here: chain text such as a proposal title never pings. |

## PagerDuty Settings

| Config Setting | Description |
| --- | --- |
| `pagerduty.enabled` | Use PagerDuty. If no, it overrides chain settings. |
| `pagerduty.api_key` | An Events API v2 integration key, see [pagerduty.md](pagerduty.md). |
| `pagerduty.default_severity` | Not currently used. |

## Discord Settings

| Config Setting | Description |
| --- | --- |
| `discord.enabled` | Alert to discord. If no, it overrides chain settings. |
| `discord.webhook` | See [discord.md](discord.md). |
| `discord.style` | `embed` (default) is a block with a color down the side, `plain` is an ordinary message. |

## Telegram Settings

| Config Setting | Description |
| --- | --- |
| `telegram.enabled` | Alert via telegram. If no, it overrides chain settings. |
| `telegram.api_key` | Bot token from @BotFather, see [telegram.md](telegram.md). |
| `telegram.channel` | Chat id, see [telegram.md](telegram.md). |
| `telegram.commands` | Answer `/status`, `/report` and `/mentionme`. Off by default, see [telegram.md](telegram.md). |
| `telegram.admins` | Telegram user ids allowed to send commands in a group. |

## Slack Settings

| Config Setting | Description |
| --- | --- |
| `slack.enabled` | Alert to Slack. If no, it overrides chain settings. |
| `slack.webhook` | Incoming webhook URL, added in the Slack app directory. |

## Quiet Hours

Alerts below `min_severity` wait until the quiet hours end, and are sent then if still active. Alerts at or above it are sent at once.

| Config Setting | Description |
| --- | --- |
| `quiet_hours.enabled` | Hold back quieter alerts. |
| `quiet_hours.start` | Start, `23:00`. The range may cross midnight. |
| `quiet_hours.end` | End, `07:00`. |
| `quiet_hours.timezone` | IANA name (`Europe/Berlin`, follows daylight saving) or fixed offset (`UTC+7`, `UTC-05:30`). UTC when empty. |
| `quiet_hours.min_severity` | Severity sent anyway. Critical when empty. |

With `critical`, jailing, a stopped chain, all RPC nodes failing, consecutive misses and jail risk still wake you. Governance proposals, upgrades, missed proposals and being able to unjail wait. `percentage_priority` is `warning` by default, so set it to `critical` if percentage alerts should wake you.

## Health Check Settings

| Config Setting | Description |
| --- | --- |
| `healthcheck.enabled` | Send pings to show the monitor is running. |
| `healthcheck.ping_url` | URL to ping. Only HTTP 2xx counts as success. |
| `healthcheck.ping_rate` | Seconds between pings. |

## Fibre Metrics

Receives the OTLP/HTTP metrics a Celestia Fibre server exports. Off unless `listen` is set. Setup and alerts are in [fibre.md](fibre.md); the per-chain alert settings are in [chains.md](chains.md). A request over 1 MiB (4 MiB unzipped) is refused, and past 4 decoded at once the rest get a 503, which exporters retry.

| Config Setting | Description |
| --- | --- |
| `fibre_metrics.listen` | `host:port` to listen on, empty is off. `127.0.0.1:4318` takes metrics from Fibre on the same machine. |
| `fibre_metrics.token` | Any long random string you choose, e.g. `openssl rand -hex 32`; Fibre sends it as `Authorization: Bearer <token>`. Required unless `listen` is a loopback address. Behind Caddy or nginx set it too, or at least let only Fibre's IP through, see [fibre.md](fibre.md#setup). On a non-loopback `listen` without `tls_cert` it travels in cleartext, tenderduty warns. |
| `fibre_metrics.allow_without_token` | Accept metrics without a token on a non-loopback listen. Only when a firewall lets just Fibre's IP reach the port. Not recommended. Default no. |
| `fibre_metrics.tls_cert` | Certificate file, serves HTTPS with `tls_key`. Both or neither. |
| `fibre_metrics.tls_key` | Private key file for `tls_cert`. |
| `fibre_metrics.export_raw` | Serve every metric Fibre exports on the Prometheus endpoint, see [fibre.md](fibre.md#raw-export). Default `no`. |

## Reports

```yaml
reports:
  daily: "09:00"
  weekly: "monday 09:00"
  timezone: Asia/Jakarta
  include_testnets: yes
```

Empty `daily` or `weekly` turns that report off. `timezone` takes an IANA name or an offset like `UTC+7`; it defaults to the quiet hours timezone, then UTC. Applies on reload.

- Reports show signing, voting power and rank changes, delegators, incidents, votes due, scheduled upgrades and Fibre totals. Weekly reports compare with the previous week. Unknown numbers say `unavailable`.
- Each enabled Telegram, Discord and Slack destination gets one report for its chains. No mentions, quiet hours don't apply, PagerDuty gets none.
- Testnets are included by default. Set `testnet: yes` or `no` in a chain file to override the registry's `network_type`; unknown chains count as mainnet.
- With Telegram commands on, `/report` and `/report weekly` show the current report.
- Reports missed while tenderduty was down are sent up to six hours late, marked `(late)`.
- The state file keeps 30 days of history for this. Restart downtime is not counted as watched time.
