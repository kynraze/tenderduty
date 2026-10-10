# Settings for a chain

Each chain goes in its own file in `chains.d`, named after the chain: `chains.d/Osmosis.yml` is "Osmosis". The file holds the settings below directly, with no `chains:` section. See [examples/chains.d](../examples/chains.d), or run `tenderduty -example-chain`.

Shared settings, destinations and `alert_defaults` are in [config.md](config.md).

* [A minimal chain](#a-minimal-chain)
* [Chain Settings](#chain-settings)
* [Alerting Settings](#alerting-settings)
* [Node Settings](#node-settings)
* [Several validators on one chain](#several-validators-on-one-chain)
* [Chain commands](#chain-commands)

Booleans can be `true/false` or `yes/no`. Alert defaults apply to omitted alert settings; other omitted settings are an empty string, zero, false or nil.

## A minimal chain

Include the settings that differ from `alert_defaults`:

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
| `public_fallback` | Fall back to a public RPC node from the [chain registry](https://cosmos.directory), testnets included, if all your nodes fail. Use public nodes as a backup. |
| `testnet` | `yes` or `no` overrides the registry's `network_type` for [reports](config.md#reports). Unknown chains count as mainnet. |
| `extra_info` | Text added to every alert for this chain, e.g. which instance sent it. PagerDuty gets it in the custom details. |

## Alerting Settings

These go under `alerts`. Anything left out comes from `alert_defaults`. Set any alert switch to `no` to turn it off. Severities are configurable only for missed block proposals, governance and Fibre (`missed_proposal_severity`, `gov_severity`, `fibre_severity`), and for node down with `node_down_alert_severity` in [config.md](config.md#general-settings); signing and upgrade alerts have fixed severities. Dates in alerts use `reports.timezone`, else `quiet_hours.timezone`, else UTC.

| Config Setting | Description |
| --- | --- |
| `missed_blocks_enabled` | One alert per validator: `consecutive_missed` blocks in a row (warning), close to jail (critical), then jailed (`jailed since ... · can unjail from ...`), tombstoned or not in the active set (critical). Stage changes replace the previous stage silently, with one recovery at the end. On by default. |
| `consecutive_missed` | Blocks missed in a row before the warning. 5 when empty. It recovers after 2 minutes without another run, a single missed block does not hold it. |
| `jail_risk_percent` | Percent of allowed misses used before the critical alert, even with intermittent misses. 90 when empty. |
| `jail_risk_minutes` | Also alert critically if, while missing blocks, jailing is within this many minutes at the chain's block time. Needs `consecutive_missed` in a row until half the window is used. 60 when empty. |
| `stalled_enabled` | Alert critically when the chain stops producing blocks. On by default. |
| `stalled_minutes` | Minutes without a block before alerting. 10 when empty. |
| `alert_if_no_servers` | Alert critically if no RPC servers respond (uses `node_down_alert_minutes`). On by default. |
| `missed_proposal_enabled` | Alert on missed block proposals: the validator was due to propose but another validator did. Off by default. |
| `missed_proposal_count` | How many within `missed_proposal_minutes` trigger it. 3 when empty. |
| `missed_proposal_minutes` | The window in minutes. 60 when empty. |
| `missed_proposal_severity` | `info`, `warning`, `error` or `critical`. Warning when empty. |
| `gov_enabled` | Message when a proposal opens for voting, cleared when the valoper's account votes. On by default. Proposals already in voting when a chain is first watched come as one summary. |
| `gov_remind_minutes` | Remind when voting ends within this many minutes without a vote. 60 when empty. |
| `gov_early_remind_percent` | Warning at this percent of voting time left. Default 25; 0 is off, 1-99 valid. Other values warn and use 25. Skipped without a start time or within 2 hours of voting start or the final reminder. |
| `gov_severity` | Info when empty, early reminders are warning, final reminders critical. |
| `upgrade_enabled` | Scheduled x/upgrade alerts and Celestia x/signal version signalling. Scheduled upgrades and reminders are critical; a signal not sent is warning. Upgrade alerts show the estimated date and time. Chains without x/signal are skipped. On by default. |
| `upgrade_remind_minutes` | Remind this many minutes before the x/upgrade plan. 60 when empty. |
| `signal_percent` | Celestia: share of voting power that must have signalled before warning that this validator has not. 5 when empty. |
| `signal_remind_percent` | Celestia: critical reminder when this share has signalled and this validator has not. 66 when empty. Ignored when not above `signal_percent`. |
| `signal_remind_minutes` | Celestia: remind this many minutes before the scheduled upgrade if any own node is not ready. 60 when empty. Versions are read from nodes with `alert_if_down`; without those, from the node in use, and a chain stuck at the upgrade height alerts `Node stopped at the version N upgrade height`. A stopped own node's down alert names the upgrade. |
| `bridge_enabled` | Bridge RPC alerts, off by default. Requires `bridge.url` in this chain file. See [bridge.md](bridge.md). |
| `bridge_severity` | Bridge alert severity, default `warning`. |
| `bridge_lag_blocks` | Header and sampling lag threshold, default 20 blocks. |
| `bridge_lag_minutes` | Continuous lag window, default 5 minutes. |
| `fibre_enabled` | Celestia Fibre: alert while the validator is not registered, its Fibre host is unreachable (TLS handshake fails for `node_down_alert_minutes`), or the host's certificate isn't this validator's. With [`fibre_metrics`](config.md#fibre-metrics) it also alerts on what Fibre reports. Off by default. See [fibre.md](fibre.md). |
| `fibre_instance` | The hostname Fibre reports as `service.instance.id`. Needed when more than one chain has `fibre_enabled`. |
| `fibre_storage_percent` | Alert when Fibre's storage is at this percent of its budget or more, or an upload was turned away. 1 to 100, 0 or empty is off. |
| `fibre_idle_minutes` | Alert when Fibre stored no upload for this many minutes. 0 or empty is off. Only for steady uploads. |
| `fibre_severity` | Warning when empty. Use `critical` to receive Fibre alerts during quiet hours. |
| `pagerduty.*` | Same as `pagerduty` in `config.yml`, for this chain only. A blank api_key uses the shared one. |
| `discord.*` | Same as `discord` in `config.yml`. A blank webhook uses the shared one. |
| `telegram.*` | Same as `telegram` in `config.yml`. A blank api_key and channel use the shared ones. |
| `slack.*` | Same as `slack` in `config.yml`. A blank webhook uses the shared one. |

### Older keys

Jail alerts show nonzero slashes; tombstone alerts add double-sign evidence and kept/dropped blocks when available.

`consecutive_enabled: no` turns off the first stage of `missed_blocks_enabled`, `alert_if_inactive: no` the third (jailed or out of the active set). `percentage_*` is accepted and ignored with a load warning, `jail_risk_percent` replaces it. `consecutive_priority` and `percentage_priority` are accepted and ignored with a load warning. Other older keys still work: `consecutive_*`, `alert_if_inactive`, `alert_if_no_servers`, `stalled_*`.

A destination is only reached when it is enabled in `config.yml`; the chain follows that unless it sets `enabled` itself.

## Node Settings

Nodes are tried in order until one answers, so put your own first. Without `nodes` and with `public_fallback`, tenderduty uses only public nodes. These can be unreliable and use other validators' resources. A node that refuses `/websocket` counts as down, some proxies don't pass it through.

| Config Setting | Description |
| --- | --- |
| `nodes[]` | List of RPC servers. |
| `nodes[].url` | `protocol://hostname:port`. Without a port https uses 443 and http 80. Only http (or tcp) and https with a valid certificate. |
| `nodes[].alert_if_down` | Alert if this node isn't responding (uses `node_down_alert_minutes`). Marks the node as your own: its celestia-app version is shown in the dashboard and `/status` and checked for upgrades. |
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

`tenderduty chain add realiovaloper1... --node http://node-b:26657 --to Realio` adds a validator to an existing file and `tenderduty chain remove Realio --validator RIO` removes one. The name comes from `--label`, then the validator's multistaking token symbol (`RIO`), then its moniker.

## Chain commands

```shell
tenderduty chain add atonevaloper1... --node https://rpc.example.com --set public_fallback=yes --set alerts.fibre_enabled=yes
tenderduty chain set Celestia alerts.fibre_enabled=yes alerts.telegram.min_severity=warning
tenderduty chain list
tenderduty chain remove Celestia
# Skip confirmation:
tenderduty chain remove Celestia --yes
```

- `tenderduty -example-celestia` prints a Celestia chain file with the Fibre settings commented. `add` does the same for Celestia chains (celestia, mocha, arabica) and prints the `chain set` command that turns Fibre on.
- `add` needs `--node`, your own synced node. It checks the node and validator, then prints the moniker, status and voting power (`LinkPool on mocha-5: active, not jailed, voting power 1,100,105`). A consensus (`valcons`) address is accepted if it is in the active set.
- `--chain osmosis` takes a registry path, mainnet ([cosmos.directory](https://cosmos.directory)) or testnet ([testcosmos.directory](https://testcosmos.directory)); its chain id and prefix must match your node and validator. `--name Celestia` sets the file name, otherwise the registry name or chain id. Names can't contain periods or path separators. An existing file needs `--force`. Your input is checked before anything is fetched.
- `--set key=value` (repeatable on `add`, positional on `set`) uses chain YAML paths. Booleans take yes/no/true/false, lists are comma separated: `alerts.telegram.mentions=alice,bob`. Unknown keys suggest the closest one. Edit `nodes` in the file; `add --to` and `remove --validator` manage `validators`.
- `add --to` on a single-validator file turns it into a `validators` list, so its alerts get the validator name (`Realio / RST`) and the old ones resolve as no longer watched.
- `set` keeps comments and key order, prints `key: old -> new` per change (`already X` for a key that has that value, in which case the file is left alone), and validates before writing.
- `set` and `remove` only edit `chains.d/<Name>.yml`, not chains in `config.yml`.
- `list` prints a table: NAME, CHAIN ID, VALIDATOR, NODES, FALLBACK, ALERTS, FILE. ALERTS lists enabled families: `missed_blocks,stalled,no_servers,gov,upgrade,missed_proposal,fibre`.
- `-f`, `-cc` and `-state` work before the command or after the subcommand.
- After a change tenderduty reloads the running process found through the state file's PID, and says `Applied: reloaded tenderduty (pid N)`, `Not applied yet: tenderduty is not running; ...` or `Saved, but reload failed: ...`. New files inherit the directory's group and use mode 0640 when the directory is group-readable. Replaced files keep their owner and mode.
