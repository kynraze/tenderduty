# Dependency upgrades

## What changed

| Component | Before | Now |
| --- | --- | --- |
| Go | 1.18 (Docker 1.19) | 1.27.1 |
| YAML parser | `github.com/go-yaml/yaml` v2 | `go.yaml.in/yaml/v3` v3.0.5 |
| Prometheus client | v1.12.2 | v1.24.1 |
| PagerDuty | v1.5.1 | v1.8.0 |
| gorilla/websocket | v1.5.0 | v1.5.3 |
| gRPC | v1.50.1 | v1.84.0 |
| protobuf | v1.28.2 | v1.36.12 |
| `golang.org/x/*` | 2022 releases | latest |
| Docker image | Debian 11, UPX packed | Debian 13 slim, static binary |
| CI actions | checkout v2/v3/v4, setup-go v5, CodeQL v2, Docker actions from 2022, gosec `master` | checkout v7, setup-go v7, CodeQL v4, Docker actions latest (pinned by commit), gosec v2.29.0 |

- Configs load the same as before: `yes/no` and `on/off` still work, unknown settings and repeated keys are only a warning (a repeated key keeps its last value).
- Go 1.27 needs TLS 1.2 or newer for HTTPS RPC nodes and webhooks, very old TLS 1.0/1.1 endpoints won't connect.
- `healthcheck.ping_rate` is still in seconds, metric names are unchanged.
- The Docker image runs as UID/GID 26657 like before.

`govulncheck` went from 70 advisories to 15, and gosec is clean.

## Still to do

All remaining advisories come from `cosmos-sdk` v0.45.11 (EOL) and `tendermint` v0.34.24, or what they pull in (`btcd`, `x/crypto/openpgp`). They can't be updated on their own, newer versions change the APIs tenderduty uses. Two are reachable from tenderduty's code: [GO-2024-3339](https://pkg.go.dev/vuln/GO-2024-3339) and [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932).

The fix is to stop depending on them: tenderduty only needs a few RPC calls (`status`, `abci_query`), the staking/slashing query types and bech32. Those can come from a small RPC client and `cosmossdk.io/api`, which would drop the SDK and Tendermint entirely. This needs testing against the chain versions people run.

Smaller items:

- Replace `textileio/go-threads`, only its broadcast package is used by the dashboard.
- Move to YAML v4 once it's stable.
