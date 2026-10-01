# Installing

* [Docker](#docker-container)
* [Docker Compose](#docker-compose)
* [Build From Source](#building-from-source)
* [Systemd Service](#run-as-a-systemd-service-on-ubuntu)

Contributions and corrections are welcomed here. Would be nice to add a section on Akash deployments too.

## Docker Container

The examples bind the dashboard and metrics ports to localhost. For access from another machine, use an SSH tunnel or an authenticated reverse proxy. The dashboard itself does not provide a login.

```shell
mkdir tenderduty && cd tenderduty
docker run --rm ghcr.io/blockpane/tenderduty:latest -example-config >config.yml
# edit config.yml and add chains, notification methods etc.
docker run -d --name tenderduty -p "127.0.0.1:8888:8888" -p "127.0.0.1:28686:28686" --restart unless-stopped -v $(pwd)/config.yml:/var/lib/tenderduty/config.yml ghcr.io/blockpane/tenderduty:latest
docker logs -f --tail 20 tenderduty
```

## Docker Compose

```shell
mkdir tenderduty && cd tenderduty

cat > docker-compose.yml << EOF
---
version: '3.2'
services:

  v2:
    image: ghcr.io/blockpane/tenderduty:latest
    command: ""
    ports:
      - "127.0.0.1:8888:8888" # Dashboard
      - "127.0.0.1:28686:28686" # Prometheus exporter
    volumes:
      - home:/var/lib/tenderduty
      - ./config.yml:/var/lib/tenderduty/config.yml
    logging:
      driver: "json-file"
      options:
        max-size: "20m"
        max-file: "10"
    restart: unless-stopped

volumes:
  home:
EOF

docker-compose pull
docker run --rm ghcr.io/blockpane/tenderduty:latest -example-config >config.yml

# Edit the config.yml file, and then start the container
docker-compose up -d
docker-compose logs -f --tail 20
```

## Building from source

*Note: building tenderduty requires go v1.18 or later*

### Installing Go

If you intend to build from source, you will need to install Go. There are many choices on how to do this. **The most common method is to use the official installation instructions at [go.dev](https://go.dev/doc/install),** but there are a couple of shortcuts that can also be used:

Ubuntu:
```shell
sudo apt-get install -y snapd
sudo snap install go --classic
```

MacOS: using [Homebrew](https://brew.sh)
```shell
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
brew install go
```

### Building

Clone the repository before building because its dependencies use replace directives in `go.mod`.

```
git clone --branch main https://github.com/kynraze/tenderduty.git
cd tenderduty
cp example-config.yml config.yml
# edit config.yml with your favorite editor
go mod download
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
```

## Run as a systemd service on Ubuntu

This runs one Tenderduty process for all configured chains. Go and Git are needed to build the binary; Go is not needed by the installed service. Install Go using the instructions above, then build this fork:

```shell
git clone --branch main https://github.com/kynraze/tenderduty.git
cd tenderduty
go mod download
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
```

Create the service user and install the binary and configuration directories. If the `tenderduty` user already exists, skip the `useradd` command.

```shell
sudo useradd --system --user-group --home-dir /var/lib/tenderduty --shell /usr/sbin/nologin tenderduty
sudo install -m 0755 tenderduty /usr/local/bin/tenderduty
sudo install -d -m 0750 -o root -g tenderduty /etc/tenderduty /etc/tenderduty/chains.d
sudo install -m 0640 -o root -g tenderduty example-config.yml /etc/tenderduty/config.yml
```

Edit `/etc/tenderduty/config.yml` with the shared settings. When using separate chain files, remove the example `chains:` section from this file. For example:

```yaml
enable_dashboard: yes
listen_port: 8888
hide_logs: yes
node_down_alert_minutes: 3

telegram:
  enabled: yes
  api_key: "YOUR_BOT_TOKEN"
  channel: "YOUR_CHAT_ID"

alert_defaults:
  consecutive_enabled: yes
  consecutive_missed: 5
  alert_if_inactive: yes
  alert_if_no_servers: yes
  stalled_enabled: yes
  stalled_minutes: 10
  telegram:
    enabled: yes
```

Create one file per chain, such as `/etc/tenderduty/chains.d/Osmosis.yml`. Replace the validator address and RPC URL with your own values:

```yaml
chain_id: osmosis-1
valoper_address: "YOUR_OSMOSIS_VALIDATOR_ADDRESS"
nodes:
  - url: "https://YOUR_OSMOSIS_RPC"
```

Chain files start directly with `chain_id`; they do not need a `chains:` wrapper. Each file inherits `alert_defaults` and can override selected fields under `alerts`. See the [configuration guide](config.md#shared-configuration).

After creating the chain files, set their ownership and permissions, install the supplied service unit, and start it:

```shell
sudo chown root:tenderduty /etc/tenderduty/chains.d/*.yml
sudo chmod 0640 /etc/tenderduty/chains.d/*.yml
sudo install -m 0644 contrib/tenderduty.service /etc/systemd/system/tenderduty.service
sudo systemd-analyze verify /etc/systemd/system/tenderduty.service
sudo systemctl daemon-reload
sudo systemctl enable --now tenderduty
sudo systemctl status tenderduty --no-pager
```

The supplied unit uses these locations:

```text
/usr/local/bin/tenderduty
/etc/tenderduty/config.yml
/etc/tenderduty/chains.d/*.yml
/var/lib/tenderduty/.tenderduty-state.json
```

`StateDirectory=tenderduty` creates the writable state directory for the service user, while `ProtectSystem=strict` keeps the rest of the filesystem read-only. The process runs without root privileges and systemd restarts it if it exits. No service needs to be installed on the remote validator servers.

Use the journal to follow logs, and restart the service after changing configuration:

```shell
sudo journalctl -u tenderduty -f
sudo systemctl restart tenderduty
```

The dashboard listens on port 8888 when enabled and does not provide a login. Use a firewall, SSH tunnel, or authenticated reverse proxy to control access. To view it through an SSH tunnel from your computer:

```shell
ssh -L 8888:127.0.0.1:8888 YOUR_USER@YOUR_MONITOR_SERVER
```

Then open `http://127.0.0.1:8888` locally.
