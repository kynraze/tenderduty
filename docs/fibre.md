# Celestia Fibre

Tenderduty watches a validator's Fibre provider in two ways, both turned on per chain with `fibre_enabled` ([chains.md](chains.md)):

- It asks the chain for the validator's registered Fibre host and dials it, without further setup. Chains without x/valaddr are skipped.
- With [`fibre_metrics`](config.md#fibre-metrics), it receives Fibre's OTLP/HTTP exports and can detect signing or disk failures even when Fibre answers.

Run tenderduty on a different machine from Fibre. The check then sees what Fibre's clients see, and still reports when Fibre's machine goes down.

## Alerts

All use the chain's `fibre_severity`. Each alert shows its numbers on the second line and describes the recovery when it resolves.

| Alert | Meaning | What to do |
| --- | --- | --- |
| Validator is not a registered Fibre provider | The chain has no Fibre host for this validator. | Register the Fibre host. |
| Fibre at `host` is unreachable | TLS handshake to the registered host fails for `node_down_alert_minutes`. The message says why. | Check that Fibre runs, the firewall and DNS. |
| Fibre at `host` does not prove it belongs to this validator | The certificate isn't signed by this validator's consensus key. | Check the registered host points at your Fibre, which uses your validator's key. |
| Fibre has sent no metrics | No export for `node_down_alert_minutes`. | Check the Fibre process, its `--otel-endpoint` and the token. |
| Fibre could not sign payment promises | More than 1% of signatures failed. Fibre needs your validator key (privval, TMKMS or Horcrux) for every upload. | Check the signer is running, has the key and reaches Fibre. |
| Fibre signing is slower than 10ms | Fewer than half of the successful signatures (at least 20) took 10ms or less, as Fibre requires. | Check the signer's latency, usually a remote signer on a slow link. |
| Fibre storage operations are failing | A store put or prune failed in the last 10 minutes. Failed gets are not counted: Fibre reports a shard it doesn't hold as a failed get. | Check disk space, health and permissions of Fibre's data directory. |
| Fibre storage backend requests are failing | More than 1% of reads from the storage backend (`local` or an object store) timed out, were throttled or failed. Not found and canceled reads don't count. | Local: check Fibre's data disk: space, health and permissions. Object: check the bucket's credentials, rate limits, network, and that it is in the same region as Fibre. |
| Fibre shard budget is nearly full | Optional: needs `fibre_storage_percent`. Budget nearly used or uploads turned away. | Nothing to fix on your side, see [the shard budget](#the-shard-budget). |
| Fibre is not endorsing | Optional: `fibre_endorsement_alert: yes`. No signature for at least `fibre_endorsement_blobs` (50) blobs and `fibre_endorsement_minutes` (120) minutes. Suppressed while an own-node down alarm is active or the validator is jailed or not bonded. | Check that Fibre can reach celestia-appd (app gRPC and signer). |
| Fibre has had no successful upload | Optional: needs `fibre_idle_minutes`. Only for steady uploads. | Check that Fibre is reachable and registered. |

Fibre v10.4 reports its storage backend (`local` or an object store such as `s3`) and the time of its last successful upload. Tenderduty shows the backend as reported (`object` when Fibre sends no name, as v10.2 did) and uses that time for the last upload and `fibre_idle_minutes`, so it survives a tenderduty restart. With an older Fibre, or before its first upload, it uses the last upload it saw itself.

Rates use the last 10 minutes, extending up to 24 hours to reach 20 samples on a quiet network. A failure can keep an alert active until 20 newer samples arrive. After a tenderduty restart, active alerts wait for an export that shows recovery. Metrics route by `service.namespace` = `chain_id` when set, else by hostname via `fibre_instance`. Unclaimed metrics are ignored; see [chains.md](chains.md).

## Setup

Fibre sends OTLP/HTTP metrics to `--otel-endpoint`. Tenderduty listens; Fibre opens no port. Set `--otel-endpoint` to the base URL. Tenderduty accepts any path ending in `/v1/metrics`.

Set `OTEL_RESOURCE_ATTRIBUTES="service.namespace=<chain_id>"` in Fibre's environment (recommended). With two Fibres on one host, label at least one. With several validators on the same chain, set `fibre_instance` to each hostname. Without a namespace, routing uses the hostname via `fibre_instance`, or the sole enabled chain that leaves it empty and no labeled Fibre feeds.

The token is any long random string you choose, set in tenderduty and in Fibre's environment as `OTEL_EXPORTER_OTLP_HEADERS` (the standard OpenTelemetry variable, Fibre has no flag for it). For example `openssl rand -hex 32`.

On the same server, loopback needs no token:

```yaml
# config.yml
fibre_metrics:
  listen: 127.0.0.1:4318
```

```
OTEL_RESOURCE_ATTRIBUTES="service.namespace=mocha-5" \
  fibre start --otel-endpoint http://127.0.0.1:4318
```

Different server on a private network (WireGuard, VPN, a provider's private LAN): listen on the private address with a token, and allow only Fibre's IP to port 4318.

```yaml
fibre_metrics:
  listen: 10.0.0.5:4318
  token: <token>
```

```
OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer <token>" \
OTEL_RESOURCE_ATTRIBUTES="service.namespace=mocha-5" \
  fibre start --otel-endpoint http://10.0.0.5:4318
```

Without a token (not recommended), restrict the port to Fibre's IP and enable unauthenticated access:

```yaml
fibre_metrics:
  listen: 0.0.0.0:4318
  allow_without_token: yes
```

```
sudo ufw allow from <fibre-ip> to any port 4318 proto tcp
```

Over the internet, put Caddy or nginx in front for HTTPS and keep `listen` on loopback. Proxied requests come from 127.0.0.1, so tenderduty can't tell they came from the internet: use a token (recommended), or at least let only Fibre's IP through the proxy. Without either, anyone can send fake metrics and hide a broken Fibre.

```
# Caddyfile, the certificate is automatic
otel.example.com {
  @fibre remote_ip 203.0.113.7   # optional without a token: only Fibre's IP
  reverse_proxy @fibre 127.0.0.1:4318
}
```

```
# nginx, certificate from certbot
server {
  listen 443 ssl;
  server_name otel.example.com;
  ssl_certificate     /etc/letsencrypt/live/otel.example.com/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/otel.example.com/privkey.pem;
  location / {
    allow 203.0.113.7;   # optional without a token: only Fibre's IP
    deny all;
    proxy_pass http://127.0.0.1:4318;
  }
}
```

```yaml
fibre_metrics:
  listen: 127.0.0.1:4318
  token: <token>   # recommended, leave out only with the IP rule above
```

```
OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer <token>" \
OTEL_RESOURCE_ATTRIBUTES="service.namespace=mocha-5" \
  fibre start --otel-endpoint https://otel.example.com
```

Listening on a public or private address directly, tenderduty requires the token unless `allow_without_token: yes`.

Without a proxy, tenderduty can serve HTTPS itself (TLS 1.2 and up). With a self-signed certificate, point Fibre at it with `OTEL_EXPORTER_OTLP_CERTIFICATE`.

```yaml
fibre_metrics:
  listen: 0.0.0.0:4318
  token: <token>
  tls_cert: /etc/tenderduty/cert.pem
  tls_key: /etc/tenderduty/key.pem
```

If Fibre already sends to a collector, forward to tenderduty from there. Other metrics are ignored.

```yaml
# OpenTelemetry Collector
exporters:
  otlphttp/tenderduty:
    endpoint: http://tenderduty:4318
    headers:
      authorization: Bearer <token>
service:
  pipelines:
    metrics:
      exporters: [prometheus, otlphttp/tenderduty]
```

```
# Grafana Alloy
otelcol.exporter.otlphttp "tenderduty" {
  client {
    endpoint = "http://tenderduty:4318"
    headers = { authorization = "Bearer <token>" }
  }
}
```

In Alloy, add `otelcol.exporter.otlphttp.tenderduty.input` to the `output` metrics list of the block that receives Fibre's metrics.

## The shard budget

The budget counts Fibre shards only; keep extra free disk for Fibre's metadata and staging.

Each blob is split into 4096 rows spread over the active set. A validator gets `rows = ceil(4096 x stake share x 3)`, at least 148 and at most 4096, and the data it receives follows its rows. These are Fibre protocol constants, not governance parameters. The budget is `full_stake_storage_budget x rows / 4096` (2 TiB on Mocha, so 74 GiB at 148 rows). Validators receive data in proportion to their budgets, so shard budgets fill at the network level. `fibre_storage_percent` is off by default because a full budget usually needs no change on one validator.

- Data is pruned after 4 hours, so use rolls under the budget.
- Set `fibre_storage_percent` (1 to 100) to alert when use reaches that share or an upload was turned away in the last 10 minutes. It clears 15 points lower with no rejected uploads for 30 minutes.
- `fibre start --unlimited-budget` removes the cap: the budget reads 0 and the alert never fires. The disk is then the limit.
- Keep at least the budget free on Fibre's data disk, ideally its own disk, and more if governance may raise the parameter.

## Endorsements

Tenderduty reads `MsgPayForFibre` in each live block and checks this validator's signature at the payment promise's validator-set height. Endorsements count blobs paid for with our signature; misses count those paid for without it. A miss usually means another validator was faster or the client did not reach this Fibre. Low endorsement reflects distance and speed to active clients and does not trigger an alert. Enable the stop alert with `fibre_endorsement_alert`; it is off by default. Both blob and minute thresholds must be met. Unknown results leave the streak unchanged, and the next endorsed blob resets it. Streaks survive restart. The dashboard shows Endorsing, Telegram shows it when stopped, and reports append the longest stop in the period. Unavailable validator history counts as unknown. Only blobs seen while tenderduty runs count; restart gaps are not backfilled.

## /status and the dashboard

`/status <chain>` in Telegram shows Fibre version, host, reachability, identity, endorsements over 24 hours, row share, shards stored with the chain budget (or unlimited; differences reported by Fibre are noted) and all known storage backends, uploads, last successful upload, signing, storage errors and metrics age. The dashboard validator detail shows provider, reachability, identity, version, row share, shards (with all known backends, including local), endorsements over 24 hours, last successful upload and metrics age, with a link to the Fibre tab. Host, endorsements, share and budget come from the chain, the rest from Fibre's metrics. Without metrics, both say so.

The dashboard has a Fibre tab beside Overview and Activity. Pick a chain and a range (1h, 24h, 7d, 30d). It has cards for status, share and shard budget, and a default first Endorsements panel with stacked endorsed/missed counts and the latest five misses, plus panels for request and internal latency (one mini chart per operation, solid p50 and dashed p95), signing, errors (the backend is named, `Storage backend (local)`), uploads and downloads, and resources (heap and stack, GC goal, memory limit only when set; goroutines, GOMAXPROCS and GOGC in the header). Uploads and downloads show ok, failed and % ok for the range. Failed uploads include payments Fibre refused (for example the payer's escrow was too low) and uploads under the validator's `min_upload_size`; failed downloads include shards this validator doesn't hold. Latency tooltips show only the hovered operation. Errors tooltips show only errors; backend request counts and reads in flight are in Downloads. Store get misses are usually shards this validator does not hold and are not counted as errors. The cards add the storage backend and the last successful upload. The open tab refreshes when Fibre metrics arrive, at most once every 5 seconds, with a 60-second fallback. Blank intervals have no observation; observed zeros are plotted. A quiet interval's latency also counts up to 5 earlier intervals, until it has 50 operations. The interval still being counted is faded. Customize panels lets you show, hide and reorder panels and choose a count chart type (auto, bars, area, line). On 24h, 7d and 30d, count panels show stacked bars per hour, 6 hours and day; area and line styles keep the finer intervals. The browser saves these preferences. Hourly rollups are kept for 30 days in the state file; the 1h and 24h charts are kept across restarts too. Short-window summary coverage still starts at process startup.

Fibre doesn't export CPU or process RSS. Use node_exporter or process-exporter for those.

`GET /fibre?range=24h&chain=ChainName` returns the same data as JSON (all watched chains without `chain`). Points include `endorsements_endorsed`, `endorsements_missed` and `endorsements_unknown` counts in `metrics`; the summary includes `last_24h.endorsed`, `last_24h.missed`, `last_24h.unknown` and `latest_missed` (up to 20). Endorsements and missed blobs stay visible with `hide_logs`. Daily and weekly Fibre reports include the endorsement rate.

## Prometheus and Grafana

With `prometheus_enabled: yes`, Fibre metrics join the `/metrics` endpoint with the labels `name`, `chain_id` and `moniker`. All names start with `tenderduty_fibre_`. Counters and histograms reset when tenderduty restarts. Use `rate` or `increase`.

- Gauges: `rows`, `rows_total`, `row_share` (0 to 1), `chain_budget_bytes`, `budget_bytes` (0 is unlimited), `occupancy_bytes`, `registered`, `reachable`, `identity_verified`, `metrics_age_seconds`, `uploads_in_flight`, `downloads_in_flight`, `last_upload_timestamp_seconds`, `storage_backend_in_flight{backend}`, `go_memory_used_bytes`, `go_memory_heap_bytes`, `go_memory_stack_bytes`, `go_memory_limit_bytes` (absent without a limit), `go_gc_goal_bytes`, `go_goroutines`, `go_processor_limit`, `go_gogc`.
- Counters: `endorsements_total{result="endorsed|missed|unknown"}`, `uploads_total{result="ok|failed|rejected"}`, `upload_bytes_total`, `downloads_total{result="ok|failed"}`, `download_bytes_total`, `signatures_total{result="ok|failed"}`, `signatures_fast_total`, `sign_seconds_total`, `store_errors_total{op="put|get|prune"}`, `storage_backend_requests_total{backend,outcome}` (success, not_found, canceled, timeout, throttled, error, unknown), `prune_entries_total`, `storage_backend_bytes_total{backend}`, `go_memory_allocated_bytes_total`, `go_memory_allocations_total`.
- Histograms (seconds, with `_bucket{le}`, `_sum`, `_count`): `upload_seconds`, `download_seconds`, `sign_seconds`, `store_put_seconds`, `store_get_seconds`, `prune_seconds`, and `go_schedule_seconds` when Fibre exports it (Fibre v10.4 doesn't, so it is absent).

Grafana: **Dashboards > New > Import**, upload [`examples/grafana/fibre.json`](../examples/grafana/fibre.json), then pick your Prometheus datasource and chain.

### Raw export

`fibre_metrics.export_raw: yes` (default `no`, needs `prometheus_enabled`) serves every metric Fibre and the bridge export on `/metrics`, named as an OpenTelemetry Collector's Prometheus exporter would (for example `fibre_server_upload_shard_duration_seconds_bucket`). The official Celestia Fibre dashboard works unchanged. Labels are the metric's attributes, the resource's (`service_name`, `service_instance_id`, `service_version`), and `name` and `chain_id`. Exponential histograms and summaries are skipped (logged). A series not exported for 10 minutes is dropped, and each chain keeps at most 20,000 series. Applies on reload. It is separate from the `tenderduty_fibre_*` metrics, so both can stay on.

Enable Fibre and print its metrics setup: `tenderduty chain fibre <Name> [--instance name]` (`--off` disables alerts).
