# TODO

- Alert when a node answers RPC but its height stops.
- Notify when a node runs an older version than the latest release.
- Alert when the bridge version is behind a scheduled upgrade.
- Add a generic webhook, ntfy and SMTP destinations.
- Document /state, /fibre and /bridge as an API.
- Add a read-only MCP server.
- Add a dashboard login.
- Post Celestia announcements from status.celestia.org.
- Read server metrics from node_exporter and CometBFT.
- Alert when the Fibre shard budget changes.
- Support several Fibre hosts per validator.
- Alert on blobs most validators endorsed but ours missed.
- Upgrade cosmos-sdk past v0.45 to clear the Dependabot alerts.
- Watch oracle misses and double-sign evidence.
- Split td2 into packages.
