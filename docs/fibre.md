# Celestia Fibre

Tenderduty watches a validator's Fibre provider in two ways. Both are turned on per chain with `fibre_enabled`, see [chains.md](chains.md).

**Run tenderduty on a different machine from Fibre.** The outside check is what Fibre's clients experience: it dials the host the validator registered, from where tenderduty runs. Run on the Fibre machine it goes through the same network and firewall as Fibre itself, and proves less. It also stops telling you anything when that machine goes down.

## What tenderduty checks

**From outside** (no setup besides `fibre_enabled`). Tenderduty asks the chain for the validator's registered Fibre host, then dials it. Chains without x/valaddr are skipped.

**From Fibre's own reports** (needs [`fibre_metrics`](config.md#fibre-metrics)). Fibre exports its metrics over OTLP/HTTP and tenderduty receives them, so it can see what the outside can't: a Fibre that answers but signs nothing, or can't write to disk.

All of these use the chain's `fibre_priority`. Every alert says the numbers behind it on its second line, and resolves with what is true now.

| Alert | What it means for the validator | What to do |
| --- | --- | --- |
| Validator is not a registered Fibre provider | The chain has no Fibre host for this validator, so clients can't find it. | Register the validator's Fibre host. |
| Fibre at `host` is unreachable | A TLS handshake to the registered host fails for `node_down_alert_minutes`. Clients can't reach Fibre. The message says why: refused, timed out, handshake failed. | Check that Fibre runs, and the firewall and DNS for the registered host. |
| Fibre at `host` does not prove it belongs to this validator | The certificate isn't signed by this validator's consensus key. Something else answers on that host. | Check the registered host points at your Fibre and Fibre uses your validator's key. |
| Fibre has sent no metrics | No export for `node_down_alert_minutes` (counted from tenderduty's start if none arrived). Fibre is down, or can't reach tenderduty. | Check the Fibre process, its `--otel-endpoint` and the token. |
| Fibre could not sign payment promises | Fibre asks your validator key (the node's privval, or TMKMS or Horcrux behind it) to sign a payment promise for every upload it stores. When that fails Fibre looks fine from outside, and the validator takes part in nothing. Raised when more than 1% of the signatures failed. | Check the signer: its connection to Fibre, that it is running and has the key. |
| Fibre signing is slower than 10ms | Fibre's README requires the median signature to take 10ms or less. Raised when fewer than half of the successful signatures did, with at least 20 of them. | Look at the signer's latency, a remote signer over a slow link is the usual cause. |
| Fibre storage operations are failing | A store put or get, or a prune, failed in the last 10 minutes. Fibre can't keep what clients upload. | Check disk space, disk health and permissions of Fibre's data directory. |
| Fibre object storage requests are failing | More than 1% of the requests Fibre makes to its object storage timed out, were throttled or failed. | Check the object storage's credentials, limits and the network to it. |
| Fibre storage is nearly full | The budget the chain gives this validator is almost used up, or uploads were turned away. Off unless `fibre_storage_percent` is set, see [the storage budget](#the-storage-budget). | Nothing on the validator's side fixes it, see below. |
| Fibre has had no successful upload | `fibre_idle_minutes` is set and the last upload was stored longer ago than that. Off unless set, and only useful when uploads are steady. | Check that Fibre is reachable and registered. |

Everything on by default is something the operator can fix. The storage alert is the one that isn't, which is why it is optional.

How the numbers are judged:

- Share rules (signing failed, object storage) look at the last 10 minutes. If that holds fewer than 20 samples they reach further back, newest first, until there are 20 or 24 hours are used up. A busy network is judged on 10 minutes, a quiet one on its last 20 samples. The second line says which: `in the last 10 minutes` or `over the last 3h 0m`.
- Because of that, one failed signature on a quiet network raises the alert and keeps it up until it falls out of the window: after 20 newer signatures, or 24 hours.
- Signing slower than 10ms looks only at successful signatures, and holds (an alert that is up stays up, one that isn't is not raised) with fewer than 20.
- Disk and prune errors raise on any failure in the last 10 minutes and clear after 10 quiet minutes.
- Fibre's counters only grow, tenderduty works out what changed between exports. What Fibre counted before tenderduty started is not news, only what happens from the first export on.
- The metrics live in memory. After tenderduty restarts, alerts that were up stay up until an export shows the problem is gone, and a quiet window counts only once tenderduty has watched all of it.
- A hostname that no chain claims is ignored and logged once. See `fibre_instance` in [chains.md](chains.md).

## The storage budget

Fibre's storage budget comes from the chain: `full_stake_storage_budget` (2 TiB) times the validator's share of the assigned rows. A validator's rows follow its stake (about three times its stake share), and the data it is sent follows the same rows. So every validator fills by the same percentage, and storage full is an event of the whole network, not of one validator.

- Data is kept for 4 hours, then pruned (the prune runs every minute), so use rolls rather than piling up.
- Governance can raise `full_stake_storage_budget`. Fibre works out its budget again every minute and picks the new value up without a restart.
- The only knob on the validator's side is `fibre start --unlimited-budget`: no cap from the chain, the disk is the limit. The budget metric is then 0 and tenderduty never raises the alert.
- Keep Fibre's data on its own disk, as Fibre's README recommends. Then a full Fibre can't take the node's disk with it.

That is why `fibre_storage_percent` is off by default. Set it (1 to 100) and the alert is raised when use reaches that share of the budget, or any upload was turned away in the last 10 minutes. It clears when use is 15 points under the threshold (under 75% for 90%) and no upload was turned away for 30 minutes that tenderduty watched, so a network hovering around the limit doesn't send a message each time it crosses the line. With a threshold of 15 or less there is no room for the gap and it clears under the threshold itself.

## Setup

4318 is the standard OTLP/HTTP port. Use another one if a collector already uses it on the machine tenderduty runs on.

Tenderduty accepts OTLP metrics as protobuf over HTTP, gzipped or not, at any path that ends in `/v1/metrics`. Traces are accepted and dropped.

### Fibre on the same server as tenderduty

```yaml
# config.yml
fibre_metrics:
  listen: 127.0.0.1:4318
```

```
fibre start --otel-endpoint http://127.0.0.1:4318
```

A loopback address needs no token. `--otel-endpoint` takes the base URL only: Fibre adds `/v1/metrics` and `/v1/traces` itself.

### Fibre on a different server

The recommended setup, see the top of this page. Listen on the address Fibre can reach, and set a token. Without one tenderduty won't start unless `listen` is a loopback address: a sender that isn't Fibre could report healthy numbers and hide a Fibre that is down.

```yaml
# config.yml
fibre_metrics:
  listen: 10.0.0.5:4318
  token: a-long-random-string
```

```
OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer a-long-random-string" \
  fibre start --otel-endpoint http://10.0.0.5:4318
```

Over the internet, serve HTTPS as well:

```yaml
fibre_metrics:
  listen: 0.0.0.0:4318
  token: a-long-random-string
  tls_cert: /etc/tenderduty/cert.pem
  tls_key: /etc/tenderduty/key.pem
```

```
OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer a-long-random-string" \
OTEL_EXPORTER_OTLP_CERTIFICATE=/path/ca.pem \
  fibre start --otel-endpoint https://tenderduty.example.com:4318
```

`ca.pem` is the CA that signed `cert.pem`, or the certificate itself when it is self-signed. TLS 1.2 and up.

### Fibre already sends to a collector

Fibre only sends to one address, so the collector forwards to tenderduty. Other apps' metrics the collector sends along are ignored. Run tenderduty with the same `fibre_metrics` as above and use a port the collector isn't using.

OpenTelemetry Collector:

```yaml
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

Grafana Alloy:

```
otelcol.exporter.otlphttp "tenderduty" {
  client {
    endpoint = "http://tenderduty:4318"
    headers = { authorization = "Bearer <token>" }
  }
}
```

Add `otelcol.exporter.otlphttp.tenderduty.input` to the `metrics` list of the output block that receives Fibre's metrics (for example `otelcol.receiver.otlp`'s `output`).
