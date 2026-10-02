# Installing

* [Docker](#docker-container)
* [Docker Compose](#docker-compose)
* [Build From Source](#building-from-source)
* [Systemd Service](#run-as-a-systemd-service-on-ubuntu)
* [Dashboard and metrics access](#dashboard-and-metrics-access)

Contributions and corrections are welcomed here. Would be nice to add a section on Akash deployments too.

## Docker Container

Opens the dashboard (8888) and metrics (28686) ports, see [dashboard and metrics access](#dashboard-and-metrics-access).

```shell
mkdir tenderduty && cd tenderduty
docker run --rm ghcr.io/kynraze/tenderduty:latest -example-config >config.yml
# edit config.yml and add chains, notification methods etc.
docker run -d --name tenderduty -p "8888:8888" -p "28686:28686" --restart unless-stopped -v tenderduty-state:/var/lib/tenderduty -v $(pwd)/config.yml:/var/lib/tenderduty/config.yml ghcr.io/kynraze/tenderduty:latest
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
    image: ghcr.io/kynraze/tenderduty:latest
    command: ""
    ports:
      - "8888:8888" # Dashboard
      - "28686:28686" # Prometheus exporter
    volumes:
      - home:/var/lib/tenderduty
      - ./config.yml:/var/lib/tenderduty/config.yml
      - ./chains.d:/var/lib/tenderduty/chains.d/
    logging:
      driver: "json-file"
      options:
        max-size: "20m"
        max-file: "10"
    restart: unless-stopped

volumes:
  home:
EOF

docker compose pull
docker run --rm ghcr.io/kynraze/tenderduty:latest -example-config >config.yml
mkdir -p chains.d

# Edit the config.yml file, and then start the container
docker compose up -d
docker compose logs -f --tail 20
```

## Building from source

*Note: building tenderduty requires go v1.27.1 or later*

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

One tenderduty process for all chains, running as its own user.

### 1. Build and install the binary

Install Go using the [instructions above](#installing-go), then:

```shell
git clone --branch main https://github.com/kynraze/tenderduty.git
cd tenderduty
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
sudo install -m 0755 tenderduty /usr/local/bin/tenderduty
```

### 2. Create the user and config directories

```shell
id tenderduty || sudo useradd --system --user-group --home-dir /var/lib/tenderduty --shell /usr/sbin/nologin tenderduty
sudo install -d -m 0750 -o root -g tenderduty /etc/tenderduty /etc/tenderduty/chains.d
```

### 3. Write the configuration

Copy the examples, then edit them:

```shell
sudo cp examples/config.yml /etc/tenderduty/config.yml
sudo cp examples/chains.d/Osmosis.yml /etc/tenderduty/chains.d/
```

- `config.yml`: notifications and `alert_defaults`.
- `chains.d/`: one file per chain, use `Realio.yml` for several validators on one chain.

Then let the service user read them:

```shell
sudo chown -R root:tenderduty /etc/tenderduty
sudo chmod -R u=rwX,g=rX,o= /etc/tenderduty
```

### 4. Start the service

```shell
sudo install -m 0644 contrib/tenderduty.service /etc/systemd/system/tenderduty.service
sudo systemctl daemon-reload
sudo systemctl enable --now tenderduty
sudo journalctl -u tenderduty -f
```

State is kept in `/var/lib/tenderduty`. Restart after changing the config: `sudo systemctl restart tenderduty`.

### Updating


```shell
git pull
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
sudo install -m 0755 tenderduty /usr/local/bin/tenderduty
sudo systemctl restart tenderduty
```

## Dashboard and metrics access

- Run tenderduty on its own small server, not on a validator, so it keeps alerting when a validator goes down.
- The dashboard has no login. It only shows on-chain info, so it's fine to share, just set `hide_logs: yes` so node addresses don't show up in the log feed.
- The prometheus metrics (28686) include node addresses, only open that port to your prometheus server.
- Using ufw? Allow SSH first. Docker skips ufw rules, so bind private ports like `"127.0.0.1:28686:28686"` instead.

```shell
sudo ufw allow OpenSSH
sudo ufw allow 8888/tcp
sudo ufw allow from YOUR_PROMETHEUS_IP to any port 28686 proto tcp
sudo ufw enable
```

For a domain with HTTPS, put a reverse proxy on the host in front of `localhost:8888`. Caddy:

```
tenderduty.example.com {
    reverse_proxy localhost:8888
}
```

nginx (with certbot for the certificate), the dashboard uses a websocket so pass the upgrade headers:

```
server {
    server_name tenderduty.example.com;
    location / {
        proxy_pass http://127.0.0.1:8888;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
    }
}
```

Want it private? Add `basic_auth` (Caddy) or `auth_basic` (nginx), or use an SSH tunnel and open `http://127.0.0.1:8888`:

```shell
ssh -L 8888:127.0.0.1:8888 YOUR_USER@YOUR_MONITOR_SERVER
```
