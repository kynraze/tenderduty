# Installing

* [Docker](#docker-container)
* [Docker Compose](#docker-compose)
* [Build From Source](#building-from-source)
* [Systemd Service](#run-as-a-systemd-service-on-ubuntu)
* [Dashboard and metrics access](#dashboard-and-metrics-access)

## Docker Container

Opens the dashboard (8888) and metrics (28686) ports, see [dashboard and metrics access](#dashboard-and-metrics-access).

```shell
mkdir tenderduty && cd tenderduty
docker run --rm ghcr.io/kynraze/tenderduty:latest -example-config >config.yml
mkdir chains.d && docker run --rm ghcr.io/kynraze/tenderduty:latest -example-chain >chains.d/Osmosis.yml
# edit config.yml for the notification methods, and chains.d/Osmosis.yml for the chain
docker run -d --name tenderduty -p "8888:8888" -p "28686:28686" --restart unless-stopped -v tenderduty-state:/var/lib/tenderduty -v $(pwd)/config.yml:/var/lib/tenderduty/config.yml ghcr.io/kynraze/tenderduty:latest
docker logs -f --tail 20 tenderduty
```

After editing the config, use `docker exec tenderduty tenderduty reload` or `docker kill -s HUP tenderduty`.

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
docker run --rm ghcr.io/kynraze/tenderduty:latest -example-chain >chains.d/Osmosis.yml

# Edit config.yml and chains.d/Osmosis.yml, and then start the container
docker compose up -d
docker compose logs -f --tail 20
```

## Building from source

Building needs Go 1.27.1 or later. Install it from [go.dev](https://go.dev/doc/install), or:

```shell
# Ubuntu
sudo apt-get install -y snapd
sudo snap install go --classic
# macOS, with Homebrew
brew install go
```

Clone the repository before building, its dependencies use replace directives in `go.mod`.

```
git clone --branch main https://github.com/kynraze/tenderduty.git
cd tenderduty
cp examples/config.yml config.yml
cp -r examples/chains.d chains.d
# edit config.yml and chains.d/Osmosis.yml with your favorite editor
go mod download
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
```

## Run as a systemd service on Ubuntu

One process for all chains, running as its own user. Build the binary as above, then:

```shell
sudo install -m 0755 tenderduty /usr/local/bin/tenderduty
# skip useradd if the tenderduty user already exists
id tenderduty || sudo useradd --system --user-group --home-dir /var/lib/tenderduty --shell /usr/sbin/nologin tenderduty
sudo install -d -m 0750 -o root -g tenderduty /etc/tenderduty /etc/tenderduty/chains.d
sudo cp examples/config.yml /etc/tenderduty/config.yml
sudo cp examples/chains.d/Osmosis.yml /etc/tenderduty/chains.d/
# edit both, then let the service user read them
sudo chown -R root:tenderduty /etc/tenderduty
sudo chmod -R u=rwX,g=rX,o= /etc/tenderduty
sudo install -m 0644 contrib/tenderduty.service /etc/systemd/system/tenderduty.service
sudo systemctl daemon-reload
sudo systemctl enable --now tenderduty
sudo journalctl -u tenderduty -f
```

- `config.yml` holds notifications and `alert_defaults`. `chains.d/` has one file per chain, see `Realio.yml` for several validators on one chain.
- [contrib/tenderduty.service](../contrib/tenderduty.service) runs `tenderduty -f /etc/tenderduty/config.yml -cc /etc/tenderduty/chains.d -state /var/lib/tenderduty/.tenderduty-state.json`, and its `ExecReload` sends SIGHUP.
- State is kept in `/var/lib/tenderduty`.
- `tenderduty chain ...` and `tenderduty reload` find `/etc/tenderduty` and `/var/lib/tenderduty` on their own.
- `--home <dir>` (or `TENDERDUTY_HOME`) can replace `-f`, `-cc` and `-state` when they sit in one directory, for example `tenderduty --home $HOME/tenderduty`.
- After editing config: `sudo systemctl reload tenderduty`. Listener and port changes need `sudo systemctl restart tenderduty`.

To update:

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

For a domain with HTTPS, put a reverse proxy in front of `localhost:8888`. Caddy:

```
tenderduty.example.com {
    reverse_proxy localhost:8888
}
```

nginx (with certbot for the certificate). The dashboard uses a websocket, so pass the upgrade headers:

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

To keep it private, add `basic_auth` (Caddy) or `auth_basic` (nginx), or use an SSH tunnel and open `http://127.0.0.1:8888`:

```shell
ssh -L 8888:127.0.0.1:8888 YOUR_USER@YOUR_MONITOR_SERVER
```
