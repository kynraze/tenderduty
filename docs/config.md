# Settings for config.yml

`config.yml` holds the settings every chain shares: the dashboard, the notification destinations, quiet hours and `alert_defaults`. Each chain goes in its own file in `chains.d`, documented in [chains.md](chains.md). See [examples/](../examples) for both.

**ProTip:** If you only have a binary, or are using the docker image, `-example-config` dumps [examples/config.yml](../examples/config.yml) to STDOUT and `-example-chain` dumps [examples/chains.d/Osmosis.yml](../examples/chains.d/Osmosis.yml). This can be used to get started without needing to download from Github. Example:

```
$ tenderduty -example-config > config.yml
$ mkdir chains.d && tenderduty -example-chain > chains.d/Osmosis.yml
```

Or if using the docker image:

```
$ docker run --rm ghcr.io/kynraze/tenderduty:latest -example-config >config.yml
$ mkdir chains.d && docker run --rm ghcr.io/kynraze/tenderduty:latest -example-chain >chains.d/Osmosis.yml
```

* [Shared configuration](#shared-configuration)
* [General Settings](#general-settings)
* [Alert Destinations](#alert-destinations)
* [Pagerduty Settings](#pagerduty-settings)
* [Discord Settings](#discord-settings)
* [Telegram Settings](#telegram-settings)
* [Slack Settings](#slack-settings)
* [Quiet Hours](#quiet-hours)
* [Health Check Settings](#health-check-settings)
* [Fibre Metrics](#fibre-metrics)
* [Settings for a chain](chains.md)

A few notes on how Go handles YAML:

* Booleans can be specified with either true/false or yes/no
* If a setting is omitted it will default to an empty string for strings, zero for numbers, false for booleans, and nil for arrays and structures.
* This can be useful for building a more compact config file.

## Shared configuration

### Shared alert settings

Set `alert_defaults` once to apply alert thresholds to every chain. A chain can include only the values that differ under its own `alerts` section. Existing configurations without `alert_defaults` keep their current behavior.

Unknown settings, missing credentials and invalid RPC URLs are logged as warnings. A `chains.d` file replaces a chain with the same name in `config.yml`.

```yaml
telegram:
  enabled: yes
  api_key: "your-bot-token"
  channel: "your-chat-id"

alert_defaults:
  consecutive_enabled: yes
  consecutive_missed: 5

chains:
  Osmosis:
    chain_id: osmosis-1
    valoper_address: osmovaloper1...
    nodes:
      - url: http://localhost:26657
    alerts:
      consecutive_missed: 3
```

Osmosis gets telegram alerts without mentioning telegram at all, see [alert destinations](#alert-destinations).

Running several validators on one chain, like Realio multistaking? See [several validators on one chain](chains.md#several-validators-on-one-chain).

## General Settings

| Config Setting               | Description                                                                                                                                                                                                       |
|------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `enable_dashboard`           | controls whether the dashboard is enabled                                                                                                                                                                         |
| `listen_port`                | What TCP port the dashboard will listen on. Only the port is controllable for now.                                                                                                                                |
| `hide_logs`                  | hide_logs is useful if the dashboard will be posted publicly. It disables the log feed, and obscures most node-related details. Be aware this isn't fully vetted for preventing info leaks about node names, etc. |
| `node_down_alert_minutes`    | How long to wait before alerting that a node is down.                                                                                                                                                             |
| `node_down_alert_severity`   | Severity sent with node down alerts, info, warning, error or critical.                                                                                                                                            |
| `prometheus_enabled`         | Should the prometheus exporter be enabled? See the [prometheus doc](prometheus.md) for information about what endpoints are available.                                                                            |
| `prometheus_listen_port`     | What port should it listen on? For now only port is configurable                                                                                                                                                  |

## Alert Destinations

Turning a destination on in `config.yml` is enough, every chain uses it. A chain only has to say anything if it wants to be left out:

```yaml
# chains.d/Osmosis.yml
alerts:
  telegram:
    enabled: no      # every other chain still gets telegram alerts
```

The shared switch has the last word: with `telegram.enabled: no` in `config.yml` no chain gets telegram alerts, whatever its own file says. Setting the destinations under `alert_defaults` still works and does the same thing, it is just no longer needed.

The older `pagerduty_alerts`, `discord_alerts` and `telegram_alerts` under a chain's `alerts` are deprecated and still honoured: they turn the destination on, the same as `pagerduty.enabled: yes`.

## PagerDuty Settings

| Config Setting               | Description                                                                                                                                                                                                       |
|------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `pagerduty.enabled`          | Should we use PD? Be aware that if this is set to no it overrides individual chain alerting settings.                                                                                                             |
| `pagerduty.api_key`          | This is an API key, not oauth token, [see the pagerduty doc](pagerduty.md) for specific setup details.                                                                                                            |
| `pagerduty.default_severity` | Not currently used, but will be soon. This allows setting escalation priorities etc.                                                                                                                              |
| `*.min_severity`             | Every destination (pagerduty, discord, telegram, slack, also per chain under alerts): only send alerts of this severity and up, info, warning, error or critical. Empty sends everything. A recovery is always sent where its alert went. |
| `*.mentions`                 | Who to ping, added to every alert (discord, telegram, slack). Discord only pings its own forms: `<@user-id>`, `<@&role-id>`, `@here`, `@everyone`. Plain text like `@someone` is not a ping.                                                  |
| `discord.style`              | `embed` (default) draws each alert as a block with a colour down the side, `plain` sends one ordinary message instead.                                                                                                                    |

## Discord Settings

| Config Setting               | Description                                                                                                                                                                                                       |
|------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `discord.enabled`            | Alert to discord? Also overrides chain-specific alerts if "no".                                                                                                                                                   |
| `discord.webhook`            | See the [discord setup document](discord.md) for how to get this information.                                                                                                                                     |

## Telegram Settings

| Config Setting     | Description                                                                         |
|--------------------|-------------------------------------------------------------------------------------|
| `telegram.enabled` | Alert via telegram? Note: also supersedes chain-specific settings.                  |
| `telegram.api_key` | API key ... talk to @BotFather. More setup info in the [telegram doc](telegram.md). |
| `telegram.channel` | See the [telegram doc](telegram.md) for how to get this value.                      |
| `telegram.commands` | Answer commands sent to the bot, `/status` and `/mentionme`. Off by default, see the [telegram doc](telegram.md). |
| `telegram.admins`  | Telegram user ids allowed to send commands in a group. Only the chat in `channel` is listened to.    |

## Slack Settings

| Config Setting   | Description                                                                         |
|------------------|-------------------------------------------------------------------------------------|
| `slack.enabled`  | Send alerts to Slack? Also overrides chain-specific alerts if "no".                 |
| `slack.webhook`  | The webhook can be added in the Slack app directory.                                |

## Quiet Hours

Holds back alerts below `min_severity` during the given hours, and sends what is still alarming once they end. Anything at or above `min_severity` is sent straight away.

| Config Setting            | Description                                                                         |
|---------------------------|-------------------------------------------------------------------------------------|
| `quiet_hours.enabled`     | Hold back the quieter alerts during the hours below?                                |
| `quiet_hours.start`       | When the quiet hours start, `23:00`. A range may cross midnight.                    |
| `quiet_hours.end`         | When they end, `07:00`.                                                             |
| `quiet_hours.timezone`    | An IANA name like `Europe/Berlin`, which follows daylight saving, or a fixed offset like `UTC+7` or `UTC-05:30`, which does not. UTC when empty. |
| `quiet_hours.min_severity`| Severity that is sent anyway, info, warning, error or critical. Critical when empty. |

With the default `critical`, what still wakes you is jailing, a chain that stopped, every RPC node failing, consecutive missed blocks and being close to jail. What waits for morning is a new governance proposal, a scheduled upgrade, missed proposals and being able to unjail.

Worth knowing: `percentage_priority` is `warning` by default, so with `percentage_enabled` turned on a chain missing blocks slowly can wait until quiet hours end. Consecutive missed blocks are critical and still page immediately, but set `percentage_priority: critical` if you want that one to as well.

## Health Check Settings

| Config Setting          | Description                                                                         |
|-------------------------|-------------------------------------------------------------------------------------|
| `healthcheck.enabled`   | Send pings to determine if the monitor is running?                                  |
| `healthcheck.ping_url`  | URL to send pings to.                                                               |
| `healthcheck.ping_rate` | Rate in which pings are sent in seconds.                                            |

## Fibre Metrics

Receives the OTLP/HTTP metrics a Celestia Fibre server exports, for the alerts that need them. Off unless `listen` is set. How to point Fibre at it, and what it alerts on, is in [fibre.md](fibre.md).

| Config Setting          | Description                                                                         |
|-------------------------|-------------------------------------------------------------------------------------|
| `fibre_metrics.listen`  | `host:port` to listen on, empty is off. `127.0.0.1:4318` takes metrics from Fibre on the same machine. |
| `fibre_metrics.token`   | When set, the sender must send `Authorization: Bearer <token>`. Required unless `listen` is a loopback address (`127.0.0.0/8`, `::1`, `localhost`): a sender that isn't Fibre could report healthy numbers and hide a Fibre that is down, so tenderduty refuses to start without one. |
| `fibre_metrics.tls_cert`| Certificate file, serves HTTPS with `tls_key`. Both or neither.                      |
| `fibre_metrics.tls_key` | Private key file for `tls_cert`.                                                    |

The alerts themselves are settings of a chain, see `fibre_enabled`, `fibre_instance`, `fibre_storage_percent` and `fibre_idle_minutes` in [chains.md](chains.md).
