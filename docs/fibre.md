# Celestia Fibre

Tenderduty watches a validator's Fibre provider in two ways, both turned on per chain with `fibre_enabled` ([chains.md](chains.md)):

- From outside, no other setup. It asks the chain for the validator's registered Fibre host and dials it. Chains without x/valaddr are skipped.
- From Fibre's own reports, with [`fibre_metrics`](config.md#fibre-metrics). Fibre exports metrics over OTLP/HTTP and tenderduty receives them, so it sees a Fibre that answers but signs nothing or can't write to disk.

**Run tenderduty on a different machine from Fibre.** The outside check should see what Fibre's clients see, and it stops telling you anything if that machine goes down.

## Alerts

All use the chain's `fibre_priority`. Each alert shows its numbers on the second line and resolves with what is true now.

| Alert | Meaning | What to do |
| --- | --- | --- |
| Validator is not a registered Fibre provider | The chain has no Fibre host for this validator. | Register the Fibre host. |
| Fibre at `host` is unreachable | TLS handshake to the registered host fails for `node_down_alert_minutes`. The message says why. | Check that Fibre runs, the firewall and DNS. |
| Fibre at `host` does not prove it belongs to this validator | The certificate isn't signed by this validator's consensus key. | Check the registered host points at your Fibre, which uses your validator's key. |
| Fibre has sent no metrics | No export for `node_down_alert_minutes`. | Check the Fibre process, its `--otel-endpoint` and the token. |
| Fibre could not sign payment promises | More than 1% of signatures failed. Fibre needs your validator key (privval, TMKMS or Horcrux) for every upload. | Check the signer is running, has the key and reaches Fibre. |
| Fibre signing is slower than 10ms | Fewer than half of the successful signatures (at least 20) took 10ms or less, as Fibre requires. | Check the signer's latency, usually a remote signer on a slow link. |
| Fibre storage operations are failing | A store put, get or prune failed in the last 10 minutes. | Check disk space, health and permissions of Fibre's data directory. |
| Fibre object storage requests are failing | More than 1% of requests to object storage timed out, were throttled or failed. | Check the credentials, limits and network. |
| Fibre storage is nearly full | Optional: needs `fibre_storage_percent`. Budget nearly used or uploads turned away. | Nothing to fix on your side, see [the storage budget](#the-storage-budget). |
| Fibre has had no successful upload | Optional: needs `fibre_idle_minutes`. Only for steady uploads. | Check that Fibre is reachable and registered. |

Rates are judged over the last 10 minutes, or the last 20 samples (up to 24 hours) on a quiet network, so one failure there stays up until 20 newer samples arrive. Alerts that were up stay up after a tenderduty restart until an export shows the fix. Metrics from a hostname no chain claims are ignored, see `fibre_instance` in [chains.md](chains.md).

## Setup

Fibre sends its metrics to `--otel-endpoint` (OTLP/HTTP); only tenderduty listens, Fibre opens no port. `--otel-endpoint` takes the base URL only, tenderduty accepts any path ending in `/v1/metrics`.

The token is any long random string you choose, set in tenderduty and in Fibre's environment as `OTEL_EXPORTER_OTLP_HEADERS` (the standard OpenTelemetry variable, Fibre has no flag for it). For example `openssl rand -hex 32`.

Same server, no token needed on loopback:

```yaml
# config.yml
fibre_metrics:
  listen: 127.0.0.1:4318
```

```
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
  fibre start --otel-endpoint http://10.0.0.5:4318
```

Without a token (not recommended), let only Fibre's IP reach the port and say so:

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

## The storage budget

Each blob is split into 4096 rows spread over the active set. A validator gets `rows = ceil(4096 x stake share x 3)`, at least 148 and at most 4096, and the data it receives follows its rows. These are Fibre protocol constants, not governance parameters. The budget is `full_stake_storage_budget x rows / 4096` (2 TiB on Mocha, so 74 GiB at 148 rows). Every validator fills by the same percentage, so a full budget is the whole network's event and there is nothing to fix on one validator. That is why `fibre_storage_percent` is off by default.

- Data is pruned after 4 hours, so use rolls under the budget.
- Set `fibre_storage_percent` (1 to 100) to alert when use reaches that share or an upload was turned away in the last 10 minutes. It clears 15 points lower with no rejected uploads for 30 minutes.
- `fibre start --unlimited-budget` removes the cap: the budget reads 0 and the alert never fires. The disk is then the limit.
- Keep at least the budget free on Fibre's data disk, ideally its own disk, and more if governance may raise the parameter.

## /status and the dashboard

`/status <chain>` in Telegram and the chain detail on the dashboard show a Fibre section for chains with `fibre_enabled`: version, host, reachable, identity, row share, budget (chain's and Fibre's), storage used, uploads in the last 10 minutes (ok, failed, turned away), last upload, signing, storage and metrics age. Host, share and budget come from the chain, the rest from Fibre's metrics. Without `fibre_metrics`, or before Fibre has sent anything, one line says so.

The dashboard has a **Fibre** tab beside Overview and Activity. Pick a chain and a range (1h, 24h, 7d, 30d). It has cards for status, share, budget and Fibre's Go memory, and panels for latency (p50/p95), signing, errors, uploads and downloads. Blank intervals mean no observation, not zero. A quiet interval's latency also counts up to 5 earlier intervals, until it has 50 operations. The interval still being counted is faded. **Customize panels** sets the chart type of count panels (auto, bars, area, line), shows, hides and reorders panels, and is saved in the browser. Hourly rollups are kept for 30 days in the state file; the short charts start fresh after a restart.

Fibre doesn't export CPU or process RSS. Use node_exporter or process-exporter for those. Set `GOMEMLIMIT` on Fibre to have the memory card show a limit.

`GET /fibre?range=24h&chain=ChainName` returns the same data as JSON (all watched chains without `chain`).

## Prometheus and Grafana

With `prometheus_enabled: yes`, Fibre metrics join the `/metrics` endpoint with the labels `name`, `chain_id` and `moniker`. All names start with `tenderduty_fibre_`. Counters and histograms reset when tenderduty restarts, use `rate` or `increase`.

- Gauges: `rows`, `rows_total`, `row_share` (0 to 1), `chain_budget_bytes`, `budget_bytes` (0 is unlimited), `occupancy_bytes`, `registered`, `reachable`, `identity_verified`, `metrics_age_seconds`, `uploads_in_flight`, `downloads_in_flight`, `last_upload_timestamp_seconds`, `object_storage_in_flight`, `go_memory_used_bytes`, `go_memory_limit_bytes` (absent without a limit), `go_gc_goal_bytes`, `go_goroutines`, `go_processor_limit`, `go_gogc`.
- Counters: `uploads_total{result="ok|failed|rejected"}`, `upload_bytes_total`, `downloads_total{result="ok|failed"}`, `download_bytes_total`, `signatures_total{result="ok|failed"}`, `signatures_fast_total`, `sign_seconds_total`, `store_errors_total{op="put|get|prune"}`, `object_storage_requests_total{outcome}` (success, not_found, canceled, timeout, throttled, error, unknown), `prune_entries_total`, `object_storage_bytes_total`, `go_memory_allocated_bytes_total`, `go_memory_allocations_total`.
- Histograms (seconds, with `_bucket{le}`, `_sum`, `_count`): `upload_seconds`, `download_seconds`, `sign_seconds`, `store_put_seconds`, `store_get_seconds`, `prune_seconds`, and `go_schedule_seconds` when Fibre exports it (Fibre v10.4 doesn't, so it is absent).

Grafana: **Dashboards > New > Import**, upload [`examples/grafana/fibre.json`](../examples/grafana/fibre.json), then pick your Prometheus datasource and chain.

### Raw export

`fibre_metrics.export_raw: yes` (default `no`, needs `prometheus_enabled`) serves every metric Fibre exports, not only the ones tenderduty uses, on `/metrics`, named as an OpenTelemetry Collector's Prometheus exporter would (for example `fibre_server_upload_shard_duration_seconds_bucket`). The official Celestia Fibre dashboard works unchanged. Labels are the metric's attributes, the resource's (`service_name`, `service_instance_id`, `service_version`), and `name` and `chain_id`. Exponential histograms and summaries are skipped (logged). A series not exported for 10 minutes is dropped, and each chain keeps at most 20,000 series. Applies on reload. It is separate from the `tenderduty_fibre_*` metrics, so both can stay on.
