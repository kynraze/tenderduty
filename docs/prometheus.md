# Prometheus Endpoint

Enable with `prometheus_enabled` and `prometheus_listen_port` in [config.md](config.md#general-settings). Metrics are on `/metrics`.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: tenderduty
    static_configs:
      - targets: ["<tenderduty-ip>:28686"]
```

It listens on all interfaces unless given a host, e.g. `127.0.0.1:28686`. It has no auth, and `endpoint` labels show node hosts even with `hide_logs`. Firewall the port to your Prometheus server.

* Chain metrics are gauges with the labels `chain_id`, `moniker` and `name`, for example `tenderduty_voting_power{chain_id="chain-id",moniker="Moniker",name="Chain Name"} 125000`. Those counted since start go back to 0 when tenderduty restarts.
* Node metrics add `endpoint`, the RPC node's URL without credentials or query.
* Celestia [Fibre](fibre.md#prometheus-and-grafana) (`tenderduty_fibre_*`) and [bridge](bridge.md#prometheus) (`tenderduty_bridge_*`) metrics are listed in their docs.

| Metric | Description |
| --- | --- |
| `tenderduty_consecutive_missed_blocks` | Consecutive missed blocks, regardless of precommit or prevote. |
| `tenderduty_endpoint_down_seconds` | Seconds a node has been unhealthy (node). |
| `tenderduty_endpoint_syncing_seconds_behind` | Seconds a node is behind the head of the chain (node). |
| `tenderduty_missed_block_window` | The missed block (slashing) window. |
| `tenderduty_missed_blocks` | Blocks missed without a precommit or prevote seen, since start. |
| `tenderduty_missed_blocks_for_window` | Missed blocks in the slashing window, regardless of precommit or prevote. |
| `tenderduty_missed_blocks_precommit_present` | Late precommits: the precommit was seen but missed the commit, since start. |
| `tenderduty_missed_blocks_prevote_present` | Prevote only: a prevote was seen but no precommit, since start. |
| `tenderduty_misses_before_jail` | Blocks that can still be missed in the window before jailing. |
| `tenderduty_missed_proposals` | Blocks the validator was due to propose but another validator did, since start. |
| `tenderduty_proposed_blocks` | Blocks proposed, since start. |
| `tenderduty_signed_blocks` | Blocks signed, since start. |
| `tenderduty_time_since_last_block` | Seconds since the previous block was finalized. Set only when a block arrives, good for averaging block times. |
| `tenderduty_time_since_last_block_unfinalized` | Seconds since the previous block, always set. Use it for stall detection. |
| `tenderduty_total_monitored_endpoints` | RPC endpoints monitored for the chain. |
| `tenderduty_total_unhealthy_endpoints` | Unhealthy RPC endpoints for the chain. |
| `tenderduty_voting_power` | Consensus voting power, 0 when not in the active set. |
| `tenderduty_voting_power_rank` | Position in the active set by voting power, 0 when not in it. |
