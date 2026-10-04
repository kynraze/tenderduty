# Tenderduty Docs

### What is tenderduty?

This is a tool for validators running tendermint nodes. It sends notifications when it detects problems.

## Detailed Documentation Topics

- [Installation](install.md)
- [Shared Configuration File Settings](config.md)
- [Settings for a chain](chains.md)
- [Setting up PagerDuty](pagerduty.md)
- [Setting up Discord](discord.md)
- [Setting up Telegram](telegram.md)
- [Prometheus Exports](prometheus.md)
- [Celestia Fibre](fibre.md)
- [Remotely Configuring Tenderduty](remote.md)
- [Running on Akash](akash.md)

## What does it do?

Monitors a validator's performance across multiple chains

- The main purpose of tenderduty is to monitor consensus state, and alert if the validator is missing blocks. This can be based on consecutive misses or on a percentage missed within the slashing window.
- Alerting if jailed, tombstoned, or inactive.
- Optional alerts for missed proposals, jail risk, governance proposals, scheduled upgrades and Celestia signalling and Fibre.
- Optional quiet hours: quieter alerts wait until morning.
- Daily and weekly reports.
- Alert destinations can be customized for each chain.
- Monitors node health:
    * Optional alerting if syncing or not responding.
    * Configurable threshold to wait before alerting on downtime
- If no nodes are alive it can fallback to using public RPC nodes.

Provides a prometheus exporter for integration with other visualization systems.<br />
Sends an alert to any of four destinations: Pagerduty, Discord, Telegram, or Slack.

A dashboard for displaying status.

- The missed block grid was heavily influenced by the uptime display on [ping.pub](https://ping.pub). *Many thanks for the inspiration!*
- The overview shows the last 48 blocks beside each validator, a chain's detail shows its last 512.
- Displays a table showing validator and node status.
- Filters to focus on chains that need attention.
- Designed intentionally for maximum density for validators on a lot of chains.
- Four themes and custom colors: **Customize** in the dashboard. Settings are saved in the browser, not in `config.yml`.
- Optionally shows a real-time stream of log messages with details about ongoing health checks.

![dashboard screenshot](dash.png)

With the dashboard on, `/health` reports that the process is alive and `/ready` that monitoring works (HTTP 503 until a validator is monitored).

## System Requirements:

* CPU: minimum of 1 core
* Memory: 128MB minimum, 256MB strongly recommended for containers.
* Bandwidth: 
  * Inbound: ~500KiB/s per-validator. Ex: monitoring 16 validators consumes 8MiB/s inbound.
  * Outbound: negligible, ~5-10 KiB/s per validator.
* Storage: ~60MB for the Docker container, ~35MB for binary alone. The cache file should be only a few KB.

## How does it work?

For each chain being monitored:

* Sets up an RPC client
  - On the initial connection it will get the validators consensus key and convert to a valcons bech32 address.
  - Occasionaly checks for validator state: active, jailed, tombstoned
  - Checks for a count of missed blocks within the jailing window.
* Uses a websocket to subscribe to NewBlock and Vote events.
  - The votes are used to determine if a pre-vote/pre-commit was sent by the validator.
  - The finalized blocks are checked for the validator's signature.
* Once/minute checks the health of all nodes by creating a new RPC client and getting the status.
* If all configured nodes are down it can use the [cosmos.directory](https://cosmos.directory) to locate a public RPC node.
* State is saved every minute and on shutdown, and restored on restart.

Dashboard:

* The dashboard provides a stream of missed block and validator status information over a websocket
* Missed blocks are displayed on a grid. This grid uses different colors to represent the validator's consensus state
  * Signed blocks are muted. This should be the nominal state.
  * Missed blocks are red, with a stroke through them in a chain's detail.
  * Not all missed blocks are the same. It is useful to know if a validator sent a pre-commit or even a pre-vote and was not included in the block. This might indicate peering issues, or even that another validator is having problems and missing pre-commits from others.
  * Bright green is a block the validator proposed.
  * Amber is a missed proposal: the validator was the proposer but another validator proposed the block. No slashing, usually a slow node or network.
  * Hollow blocks have no data.

Notifications:

* For Discord, Telegram and Slack it will only send an alert on a new alarm and when the alarm clears. Pagerduty has a little more nuance.
* Each event is one message, shown as a card: the chain and what happened, then a line per fact. A cleared alert says how long it lasted. Pagerduty gets plain text.
* Failed deliveries are retried and survive a restart. After a crash the last message may be sent twice.
* A tombstone is sent once and never cleared in a chat.
* Pagerduty:
  * Pro-tip: the alarms sent to pagerduty all use a unique "key". Pagerduty will automatically de-deduplicate alerts based on this key. If you want redundant monitoring you can run multiple instances of tenderduty alerting to pagerduty and will not get duplicate alerts.
  * Additional flapping detection is applied to pagerduty (not to discord or telegram). If a node is going up and down every few minutes it will only send an alert once in a five minute period.
