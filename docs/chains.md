# Settings for a chain

Each chain gets its own file in `chains.d`, named after the chain: `chains.d/Osmosis.yml` is watched as "Osmosis". The settings below are the whole of that file, there is no `chains:` section in it. See [examples/chains.d](../examples/chains.d), or get one with `tenderduty -example-chain`.

The shared settings, notification destinations and `alert_defaults` are in [config.md](config.md).

* [A minimal chain](#a-minimal-chain)
* [Chain Settings](#chain-settings)
* [Alerting Settings](#alerting-settings)
* [Node Settings](#node-settings)
* [Several validators on one chain](#several-validators-on-one-chain)

A few notes on how Go handles YAML:

* Booleans can be specified with either true/false or yes/no
* If a setting is omitted it will default to an empty string for strings, zero for numbers, false for booleans, and nil for arrays and structures.
* This can be useful for building a more compact config file.

## A minimal chain

Only what differs from `alert_defaults` has to be here, so a chain can be short:

```yaml
# chains.d/Osmosis.yml
chain_id: osmosis-1
valoper_address: osmovaloper1xxxxxxx...
alerts:
  consecutive_missed: 3
nodes:
  - url: tcp://localhost:26657
    alert_if_down: yes
```

It alerts to every destination enabled in `config.yml` without naming any of them, see [alert destinations](config.md#alert-destinations).

Chains can also be written inline under `chains:` in `config.yml`, the older layout. The settings are the same, one level further in: `chains."Osmosis".alerts.consecutive_missed`. A `chains.d` file replaces a chain of the same name in `config.yml`.

## Chain Settings

| Config Setting     | Description                                                                                                                                                                                  |
|--------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| file name          | The user-friendly name used for labels, taken from the file: `Osmosis.yml` is "Osmosis". Written inline instead, wrap the name in quotes to prevent YAML parsing issues if there is a space or special characters. |
| `chain_id`         | The chain-id for the chain, this is verified to match when connecting to an RPC server.                                                                                                       |
| `valoper_address`  | Hooray, in v2 we derive the valcons from abci queries so you don't have to jump through hoops to figure out how to convert ed25519 keys to the appropriate bech32 address.                     |
| `valcons_override` | The valcons address to watch, for the rare chain where it cannot be derived from the valoper. Also valid per validator under `validators`.                                                     |
| `validators`       | Several validators on one chain, used instead of `valoper_address`. See [several validators on one chain](#several-validators-on-one-chain).                                                   |
| `public_fallback`  | Should the monitor fall back to a public RPC node if all supplied nodes fail? One is picked from the [chain registry](https://cosmos.directory) that is on the right chain, synced, and accepts websockets. Public nodes aren't yours, only use this as a backup. |
| `extra_info`       | Text added to every alert for this chain, e.g. which tenderduty instance sent it. PagerDuty gets it in the custom details.                                                                     |

## Alerting Settings

These go under `alerts`. Anything left out comes from `alert_defaults` in `config.yml`.

| Config Setting            | Description                                                                                                                                                                                      |
|---------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `stalled_enabled`         | If the chain stops seeing new blocks, should an alert be sent?                                                                                                                                    |
| `stalled_minutes`         | How long a halted chain takes in minutes to generate an alarm.                                                                                                                                    |
| `consecutive_enabled`     | Most basic alarm, you just missed x blocks ... would you like to know?                                                                                                                            |
| `consecutive_missed`      | How many missed blocks should trigger a notification?                                                                                                                                             |
| `consecutive_priority`    | Severity sent with the alert, used by pagerduty and by `min_severity` routing. Critical when empty.                                                                                                |
| `percentage_enabled`      | For each chain there is a specific window of blocks and a percentage of missed blocks that will result in a downtime jail infraction. Should an alert be sent if a certain percentage of this window is exceeded? |
| `percentage_missed`       | What percentage should trigger the alert?                                                                                                                                                         |
| `percentage_priority`     | Severity sent with the alert, used by pagerduty and by `min_severity` routing. Warning when empty, which [quiet hours](config.md#quiet-hours) hold back by default.                                |
| `proposal_enabled`        | Alert on missed proposals: the validator was due to propose a block but another validator did. Off by default.                                                                                     |
| `proposal_missed`         | How many missed proposals within `proposal_minutes` should trigger a notification?                                                                                                                |
| `proposal_minutes`        | The window in minutes that missed proposals are counted over.                                                                                                                                     |
| `proposal_priority`       | Severity sent with the alert, used by pagerduty. Warning when empty.                                                                                                                              |
| `jail_risk_enabled`       | Alert when the validator gets close to being jailed for downtime. The limit comes from the chain's slashing params.                                                                                |
| `jail_risk_percent`       | How much of the misses allowed before jailing, in percent, should trigger the alert?                                                                                                               |
| `jail_risk_minutes`       | While the validator is missing blocks, alert when at the chain's block time it would be jailed within this many minutes.                                                                           |
| `jail_risk_priority`      | Severity sent with the alert, used by pagerduty. Critical when empty.                                                                                                                             |
| `gov_enabled`             | Send a message when a proposal opens for voting, cleared once the validator votes. The voter is the account of the valoper address.                                                                |
| `gov_remind_minutes`      | Remind when voting ends within this many minutes and the validator has not voted.                                                                                                                 |
| `gov_priority`            | Severity sent with new proposals, used by pagerduty. Info when empty, reminders are always critical.                                                                                               |
| `upgrade_enabled`         | Send a message when an upgrade is scheduled in x/upgrade, with its height and the time estimated from the block time.                                                                              |
| `upgrade_remind_minutes`  | Remind this many minutes before the estimated time of the upgrade.                                                                                                                                |
| `upgrade_priority`        | Severity sent with scheduled upgrades, used by pagerduty. Warning when empty, reminders are always critical.                                                                                        |
| `signal_enabled`          | Celestia's x/signal: send a message when `signal_percent` of the voting power has signalled the next version, while the validator has not signalled it, when the upgrade threshold is reached, and when the upgrade is scheduled. A chain without x/signal is logged once and not asked again, so this can be set in `alert_defaults` for every chain. |
| `signal_percent`          | The share of the voting power that must have signalled before the first two messages are sent. 10 when empty.                                                                                     |
| `signal_remind_minutes`   | Remind this many minutes before the estimated time of the scheduled upgrade. 60 when empty.                                                                                                       |
| `signal_priority`         | Severity sent with the signalling and not signalled messages, used by pagerduty. Warning when empty, the threshold and the scheduled upgrade are always critical.                                  |
| `fibre_enabled`           | Celestia Fibre: send a message while the validator is not registered as a Fibre provider, while its Fibre host is unreachable (a TLS handshake that fails for `node_down_alert_minutes`), and while the host's certificate does not prove it belongs to this validator, checked against the validator's consensus key. The last two say why: connection refused, timed out, a failed TLS handshake, or what is wrong with the certificate. All three resolve themselves, and a host that only stops answering keeps its failed certificate check until it answers again. A chain without x/valaddr is logged once and not asked again, so this can be set in `alert_defaults` for every chain. The registered host can be an IP or a DNS name. The check runs from where tenderduty runs, so running tenderduty on the Fibre machine itself proves less than running it elsewhere. With [`fibre_metrics`](config.md#fibre-metrics) also set up, it sends five more messages from what Fibre reports about itself, all things the operator can fix: it has sent no metrics, it could not sign payment promises, signing is slower than 10ms, storage operations are failing, and object storage requests are failing. Two more are optional: its storage budget is nearly full (`fibre_storage_percent`) and it has stored nothing for a while (`fibre_idle_minutes`). Each one resolves itself, see [fibre.md](fibre.md). |
| `fibre_instance`          | The hostname Fibre reports as `service.instance.id`, which is its machine's hostname. Needed when more than one chain has `fibre_enabled`, so each chain takes the metrics of its own Fibre. A chain without one takes metrics from a Fibre that no chain names, if it is the only chain with `fibre_enabled` and no `fibre_instance`. |
| `fibre_storage_percent`   | Alert when Fibre's storage use is at this percent of its budget or more, or any upload was turned away in the last 10 minutes. 1 to 100, 0 or empty is off. It clears when use is 15 points lower and nothing was turned away for 30 minutes. Optional because the budget is set by the chain from the validator's stake and the whole network fills together, there is nothing to fix on the validator's side, see [fibre.md](fibre.md#the-storage-budget). Nothing is sent for use while the budget is 0, that is Fibre running with `--unlimited-budget`. |
| `fibre_idle_minutes`      | Alert when Fibre has stored no upload for this many minutes. 0 or empty is off. Only useful when uploads are steady, a Fibre nobody uploads to would alert all the time. |
| `fibre_priority`          | Severity sent with the Fibre messages, used by pagerduty and `min_severity`. Warning when empty, set `critical` to be woken up: [quiet hours](config.md#quiet-hours) hold warnings back by default, so with `warning` a Fibre that goes down at night is reported in the morning. |
| `alert_if_inactive`       | Should an alert be sent if the validator is not in the active set: jailed, tombstoned, or unbonding?                                                                                               |
| `alert_if_no_servers`     | Should an alert be sent if no RPC servers are responding? (Note this alarm uses the `node_down_alert_minutes` setting)                                                                             |
| `pagerduty.*`             | The same structure as `pagerduty` in `config.yml`, for this chain only. Including routing to a different destination. If the api_key is blank it uses the one from `config.yml`.                   |
| `discord.*`               | The same structure as `discord` in `config.yml`, for this chain only. If the webhook is blank it uses the one from `config.yml`.                                                                   |
| `telegram.*`              | The same structure as `telegram` in `config.yml`, for this chain only. If the api_key and channel are blank it uses the ones from `config.yml`.                                                    |
| `slack.*`                 | The same structure as `slack` in `config.yml`, for this chain only. If the webhook is blank it uses the one from `config.yml`.                                                                     |

A destination is only reached when it is enabled in `config.yml`; this chain follows that unless it sets `enabled` itself. See [alert destinations](config.md#alert-destinations).

## Node Settings

*Note: if this section is omitted and public fallbacks are enabled, tenderduty will only use public endpoints. This is not encouraged for a few reasons: public nodes can be unreliable, some proxy servers do not support websockets (which td relies on for watching blocks,) and it consumes resources from other validators.*

Nodes are tried in order until one answers, so put your own first.

| Config Setting        | Description                                                                                                                                                 |
|-----------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nodes[]`             | This is an array of nodes to use as RPC servers.                                                                                                            |
| `nodes[].url`         | protocol://hostname:port, without a port https uses 443 and http 80. For now only http (tcp is an alias) and https (with a valid certificate) are supported. |
| `nodes[].alert_if_down` | Should an alert be sent if this host isn't responding? Uses the `node_down_alert_minutes` setting to determine threshold.                                  |

## Several validators on one chain

Running several validators on one chain, like Realio multistaking? List them under `validators` instead of `valoper_address`, each with its own `nodes`. Works on any chain.

```yaml
# chains.d/Realio.yml
chain_id: realionetwork_3301-1

# shared by every validator below, tried after each one's own nodes
nodes:
  - url: http://spare-rpc:26657

validators:
  - name: RIO
    valoper_address: realiovaloper1first...
    nodes:
      - url: http://node-a:26657
        alert_if_down: yes
  - name: dstrx
    valoper_address: realiovaloper1second...
    nodes:
      - url: http://node-b:26657
        alert_if_down: yes
```

- Each validator alerts on its own, as `Realio / RIO` and `Realio / dstrx`.
- A node going down alerts under its validator.
- `nodes` at the chain level are shared: every validator falls back to them when its own nodes fail, so a spare RPC only has to be written once. `alert_if_down` is ignored there, because a shared node belongs to no single validator and would page once per validator; it is logged as a warning at startup.
- Every validator needs a node of its own, a shared one, or `public_fallback`.
