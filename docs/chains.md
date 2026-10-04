# Settings for a chain

Each chain goes in its own file in `chains.d`, named after the chain: `chains.d/Osmosis.yml` is "Osmosis". The file holds the settings below directly, with no `chains:` section. See [examples/chains.d](../examples/chains.d), or run `tenderduty -example-chain`.

Shared settings, destinations and `alert_defaults` are in [config.md](config.md).

* [A minimal chain](#a-minimal-chain)
* [Chain Settings](#chain-settings)
* [Alerting Settings](#alerting-settings)
* [Node Settings](#node-settings)
* [Several validators on one chain](#several-validators-on-one-chain)
* [Chain commands](#chain-commands)

Booleans can be `true/false` or `yes/no`. An omitted setting is an empty string, zero, false or nil.

## A minimal chain

Only what differs from `alert_defaults` has to be here:

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

It alerts to every destination enabled in `config.yml`, see [alert destinations](config.md#alert-destinations).

Chains can also go inline under `chains:` in `config.yml` (the older layout), one level deeper. A `chains.d` file replaces a chain of the same name there.

## Chain Settings

| Config Setting | Description |
| --- | --- |
| file name | The chain's label: `Osmosis.yml` is "Osmosis". Inline, quote names with spaces or special characters. |
| `chain_id` | Checked against the RPC node when connecting. |
| `valoper_address` | The validator's operator address. The signing (valcons) address is looked up from the chain. |
| `valcons_override` | Valcons address to watch, for a chain where it can't be derived from the valoper. Also valid per validator. |
| `validators` | Several validators on one chain, instead of `valoper_address`. See [below](#several-validators-on-one-chain). |
| `public_fallback` | Fall back to a public RPC node from the [chain registry](https://cosmos.directory), testnets included, if all your nodes fail. Public nodes aren't yours, use it as a backup. |
| `testnet` | `yes` or `no` overrides the registry's `network_type` for [reports](config.md#reports). Unknown chains count as mainnet. |
| `extra_info` | Text added to every alert for this chain, e.g. which instance sent it. PagerDuty gets it in the custom details. |

## Alerting Settings

These go under `alerts`. Anything left out comes from `alert_defaults`. The `*_priority` settings are the severity sent, used by PagerDuty and `min_severity` routing.

| Config Setting | Description |
| --- | --- |
| `stalled_enabled` | Alert when the chain stops producing blocks. |
| `stalled_minutes` | Minutes without a block before alerting. |
| `consecutive_enabled` | Alert on consecutive missed blocks. |
| `consecutive_missed` | How many missed blocks trigger it. |
| `consecutive_priority` | Critical when empty. |
| `percentage_enabled` | Alert when a percentage of the slashing window is missed. |
| `percentage_missed` | The percentage that triggers it. |
| `percentage_priority` | Warning when empty, which [quiet hours](config.md#quiet-hours) hold back. |
| `proposal_enabled` | Alert on missed proposals: the validator was due to propose but another validator did. Off by default. |
| `proposal_missed` | How many within `proposal_minutes` trigger it. |
| `proposal_minutes` | The window in minutes. |
| `proposal_priority` | Warning when empty. |
| `jail_risk_enabled` | Alert when the validator is close to jail for downtime, using the chain's slashing params. |
| `jail_risk_percent` | Percent of the misses allowed before jailing that triggers it. |
| `jail_risk_minutes` | Also alert if, while missing blocks, it would be jailed within this many minutes at the chain's block time. |
| `jail_risk_priority` | Critical when empty. |
| `gov_enabled` | Message when a proposal opens for voting, cleared when the valoper's account votes. |
| `gov_remind_minutes` | Remind when voting ends within this many minutes without a vote. |
| `gov_priority` | Info when empty, reminders are always critical. |
| `upgrade_enabled` | Message when an upgrade is scheduled in x/upgrade, with height and estimated time. |
| `upgrade_remind_minutes` | Remind this many minutes before the upgrade. |
| `upgrade_priority` | Warning when empty, reminders are always critical. |
| `signal_enabled` | Celestia x/signal: message when `signal_percent` of the voting power has signalled the next version and this validator has not, when the upgrade threshold is reached, and when it is scheduled. Chains without x/signal are skipped, so it is safe in `alert_defaults`. |
| `signal_percent` | Share of voting power that must have signalled first. 10 when empty. |
| `signal_remind_minutes` | Remind this many minutes before the scheduled upgrade. 60 when empty. |
| `signal_priority` | Warning when empty, the threshold and the scheduled upgrade are always critical. |
| `fibre_enabled` | Celestia Fibre: alert while the validator is not a registered Fibre provider, its Fibre host is unreachable (TLS handshake fails for `node_down_alert_minutes`), or the host's certificate isn't this validator's. With [`fibre_metrics`](config.md#fibre-metrics) it also alerts on what Fibre reports. Chains without x/valaddr are skipped, so it is safe in `alert_defaults`. See [fibre.md](fibre.md). |
| `fibre_instance` | The hostname Fibre reports as `service.instance.id`. Needed when more than one chain has `fibre_enabled`, so each chain gets its own Fibre's metrics. |
| `fibre_storage_percent` | Alert when Fibre's storage is at this percent of its budget or more, or an upload was turned away. 1 to 100, 0 or empty is off. See [the storage budget](fibre.md#the-storage-budget). |
| `fibre_idle_minutes` | Alert when Fibre stored no upload for this many minutes. 0 or empty is off. Only for steady uploads. |
| `fibre_priority` | Warning when empty. Quiet hours hold warnings back, use `critical` to hear about a dead Fibre at night. |
| `alert_if_inactive` | Alert if the validator is not in the active set: jailed, tombstoned or unbonding. |
| `alert_if_no_servers` | Alert if no RPC servers respond (uses `node_down_alert_minutes`). |
| `pagerduty.*` | Same as `pagerduty` in `config.yml`, for this chain only. A blank api_key uses the shared one. |
| `discord.*` | Same as `discord` in `config.yml`. A blank webhook uses the shared one. |
| `telegram.*` | Same as `telegram` in `config.yml`. A blank api_key and channel use the shared ones. |
| `slack.*` | Same as `slack` in `config.yml`. A blank webhook uses the shared one. |

A destination is only reached when it is enabled in `config.yml`; the chain follows that unless it sets `enabled` itself.

## Node Settings

Nodes are tried in order until one answers, so put your own first. Without `nodes` and with `public_fallback`, only public nodes are used. That is discouraged: they can be unreliable, some proxies lack the websockets tenderduty needs, and they use other validators' resources.

| Config Setting | Description |
| --- | --- |
| `nodes[]` | List of RPC servers. |
| `nodes[].url` | `protocol://hostname:port`. Without a port https uses 443 and http 80. Only http (or tcp) and https with a valid certificate. |
| `nodes[].alert_if_down` | Alert if this node isn't responding (uses `node_down_alert_minutes`). |
| `nodes[].server` | Name of the server this node runs on, for [`group_node_alerts`](config.md#general-settings). Default: the IP of the node's host. |

## Several validators on one chain

For several validators on one chain, like Realio multistaking, list them under `validators` instead of `valoper_address`, each with its own `nodes`.

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
- Chain-level `nodes` are shared fallbacks. `alert_if_down` is ignored there (logged as a warning), since a shared node belongs to no single validator.
- Every validator needs a node of its own, a shared one, or `public_fallback`.

`tenderduty chain add realiovaloper1... --node http://node-b:26657 --to Realio` adds a validator to an existing file and `tenderduty chain remove Realio --validator RIO` removes one. The name comes from `--label`, else the validator's multistaking token symbol (`RIO`), else its moniker.

## Chain commands

```shell
tenderduty chain add atonevaloper1... --node https://rpc.example.com --set public_fallback=yes --set alerts.fibre_enabled=yes
tenderduty chain set Celestia alerts.fibre_enabled=yes alerts.telegram.min_severity=warning
tenderduty chain list
tenderduty chain remove Celestia
# Skip confirmation:
tenderduty chain remove Celestia --yes
```

- `add` needs `--node`, your own synced node. It checks the node and validator, then prints the moniker, status and voting power (`LinkPool on mocha-5: active, not jailed, voting power 1,100,105`). A consensus (`valcons`) address is accepted if it is in the active set.
- `--chain osmosis` takes a registry path, mainnet ([cosmos.directory](https://cosmos.directory)) or testnet ([testcosmos.directory](https://testcosmos.directory)); its chain id and prefix must match your node and validator. `--name Celestia` sets the file name, otherwise the registry name or chain id. Names can't contain periods or path separators. An existing file needs `--force`. Your input is checked before anything is fetched.
- `--set key=value` (repeatable on `add`, positional on `set`) uses chain YAML paths. Booleans take yes/no/true/false, lists are comma separated: `alerts.telegram.mentions=alice,bob`. Unknown keys suggest the closest one. Edit `nodes` in the file; `add --to` and `remove --validator` manage `validators`.
- `add --to` on a single-validator file turns it into a `validators` list, so its alerts get the validator name (`Realio / RST`) and the old ones resolve as no longer watched.
- `set` keeps comments and key order, prints `key: old -> new` per change (`already X` for a key that has that value, in which case the file is left alone), and validates before writing.
- `set` and `remove` only edit `chains.d/<Name>.yml`, not chains in `config.yml`.
- `list` prints a table: NAME, CHAIN ID, VALIDATOR, NODES, FALLBACK, ALERTS, FILE.
- `-f`, `-cc` and `-state` work before the command or after the subcommand.
- After a change tenderduty reloads the running process found through the state file's PID, and says `Applied: reloaded tenderduty (pid N)`, `Not applied yet: tenderduty is not running; ...` or `Saved, but reload failed: ...`. New files take the directory's group (0640 when it is group-readable); a replaced file keeps its owner and mode.
