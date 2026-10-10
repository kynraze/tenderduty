# Celestia bridge

Tenderduty checks one celestia-node bridge per chain every 30 seconds over JSON-RPC, with a 10-second timeout per call. `header.LocalHead` supplies the local height and time; `das.SamplingStats` supplies the sampled and network heights, catch-up status and whether sampling runs. Header lag is compared with the validator RPC's chain height. It is skipped when that height is unknown or the chain is down.

Create a read token on the bridge host:

```sh
celestia bridge auth read --p2p.network <network> --node.store <store>
```

Save the token in a file readable by tenderduty. It is read and trimmed at startup and on reload. The token is never included in status, logs, saved state or metrics. An empty `token_file` sends no Authorization header, for bridges using `--rpc.skip-auth`.

Start the bridge with `--rpc.addr 0.0.0.0` to accept remote RPC connections; the default listens only on localhost. Firewall port 26658 to tenderduty's IP. With plain HTTP, the token travels unencrypted.

In the chain file, next to `public_fallback`:

```yaml
bridge:
  url: http://203.0.113.10:26658
  token_file: /etc/tenderduty/bridge-mocha.token
alerts:
  bridge_enabled: yes
  bridge_severity: warning
  bridge_lag_blocks: 20
  bridge_lag_minutes: 5
```

Bridge alerts default off. The default lag threshold is 20 blocks for 5 continuous minutes; severity defaults to warning. Enabling alerts requires a URL. These alert keys also work in `alert_defaults`. URL, token file and token changes apply on reload.

| Alert | Condition | Recovery |
|---|---|---|
| Unreachable | Three consecutive failed ticks | Bridge OK |
| Token rejected | First HTTP 401 or 403, or a missing-permission reply; replaces unreachable | Bridge OK |
| Behind | Header lag exceeds the threshold for the configured window | Bridge is back in sync |
| Sampling stuck | Sampling is stopped, or sampling lag exceeds the threshold, for the configured window | Bridge sampling resumed |

Failed ticks suspend lag checks. Alerts use the chain's routing, mentions and quiet hours. `/status` and the dashboard's chain detail show bridge head, lag and sampled head, or reachability and token failures. No daily report changes.

A bridge stopped and started again on Mocha:

![bridge down and OK in Telegram](img/bridge-alert.png)

Prometheus exports `tenderduty_bridge_up`, `tenderduty_bridge_head`, `tenderduty_bridge_lag_blocks` and `tenderduty_bridge_sampled_head`, with `name`, `chain_id` and `moniker` labels.

Version and peers are omitted: `node.Info` and `p2p.*` require admin permission. An admin token can also send transactions; tenderduty uses only read methods.
