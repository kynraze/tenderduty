# Installing

* [Docker](#docker-container)
* [Docker Compose](#docker-compose)
* [Build From Source](#building-from-source)
* [Systemd Service](#run-as-a-systemd-service-on-ubuntu)
* [Dashboard and metrics access](#dashboard-and-metrics-access)

## Docker Container

Create the config and a chain file:

```shell
mkdir tenderduty && cd tenderduty
docker run --rm ghcr.io/kynraze/tenderduty:latest -example-config >config.yml
mkdir chains.d
docker run --rm ghcr.io/kynraze/tenderduty:latest -example-chain >chains.d/Osmosis.yml
```

Set the notification methods in `config.yml` and your validator in `chains.d/Osmosis.yml`, then start it. This opens the dashboard (8888) and keeps metrics (28686) on this machine, see [dashboard and metrics access](#dashboard-and-metrics-access).

```shell
docker run -d --name tenderduty --restart unless-stopped \
  -p 8888:8888 -p 127.0.0.1:28686:28686 \
  -v tenderduty-state:/var/lib/tenderduty \
  -v $(pwd)/config.yml:/var/lib/tenderduty/config.yml \
  -v $(pwd)/chains.d:/var/lib/tenderduty/chains.d \
  ghcr.io/kynraze/tenderduty:latest
docker logs -f --tail 20 tenderduty
```

After editing the config: `docker exec tenderduty tenderduty reload` or `docker kill -s HUP tenderduty`.

## Docker Compose

Create `config.yml` and `chains.d` as for [Docker](#docker-container), then add `docker-compose.yml` next to them:

```yaml
services:
  tenderduty:
    image: ghcr.io/kynraze/tenderduty:latest
    ports:
      - "8888:8888"    # dashboard
      - "127.0.0.1:28686:28686"  # prometheus exporter, no auth
    volumes:
      - home:/var/lib/tenderduty
      - ./config.yml:/var/lib/tenderduty/config.yml
      - ./chains.d:/var/lib/tenderduty/chains.d
    logging:
      driver: json-file
      options:
        max-size: "20m"
        max-file: "10"
    restart: unless-stopped

volumes:
  home:
```

Start it:

```shell
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

Clone and build. `go install` doesn't work because the dependencies use replace directives in `go.mod`.

```shell
git clone https://github.com/kynraze/tenderduty.git
cd tenderduty
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
```

Set up and run:

```shell
cp examples/config.yml config.yml    # set the notification methods
mkdir chains.d
./tenderduty chain add <valoper> --node <url>
./tenderduty
```

Instead of `chain add`, you can copy `examples/chains.d/Osmosis.yml` to `chains.d` and edit it.

`./tenderduty` (or `./tenderduty start`) reads `config.yml` and `chains.d` from the current directory, else `/etc/tenderduty` with the state in `/var/lib/tenderduty` (Linux and macOS). `-home`, `-f`, `-cc` and `-state` choose other paths, see `tenderduty -h`.

## Run as a systemd service on Ubuntu

One process monitors all chains under its own user. Build the binary as above, then install it and create the user (skipped if it exists):

```shell
sudo install -m 0755 tenderduty /usr/local/bin/tenderduty
id tenderduty || sudo useradd --system --user-group --home-dir /var/lib/tenderduty --shell /usr/sbin/nologin tenderduty
```

Create the config, add a chain and let the service user read them:

```shell
sudo install -d -m 0750 -o root -g tenderduty /etc/tenderduty /etc/tenderduty/chains.d
sudo cp examples/config.yml /etc/tenderduty/config.yml    # set the notification methods
sudo tenderduty chain add <valoper> --node <url> --home /etc/tenderduty
sudo chown -R root:tenderduty /etc/tenderduty
sudo chmod -R u=rwX,g=rX,o= /etc/tenderduty
```

Install and start the service:

```shell
sudo install -m 0644 contrib/tenderduty.service /etc/systemd/system/tenderduty.service
sudo systemctl daemon-reload
sudo systemctl enable --now tenderduty
sudo journalctl -u tenderduty -f
```

- [contrib/tenderduty.service](../contrib/tenderduty.service) reads `/etc/tenderduty` and keeps the state in `/var/lib/tenderduty`. `tenderduty chain ...` and `tenderduty reload` find both on their own.
- After editing the config: `sudo systemctl reload tenderduty`. Listener and port changes need `sudo systemctl restart tenderduty`.

To update:

```shell
git pull
go build -mod=readonly -ldflags '-s -w' -trimpath -o tenderduty .
sudo install -m 0755 tenderduty /usr/local/bin/tenderduty
sudo systemctl restart tenderduty
```

## Dashboard and metrics access

- Run tenderduty on its own small server, not on a validator, so it keeps alerting when a validator goes down.
- The dashboard has no login of its own. It only shows on-chain info, so sharing it is fine; set `hide_logs: yes` so node addresses stay out of the log feed. For a password, put a proxy in front, see [Private dashboard](#private-dashboard).
- The prometheus metrics (28686) include node addresses. Open that port only to your prometheus server.

With ufw, allow SSH first:

```shell
sudo ufw allow OpenSSH
sudo ufw allow 8888/tcp
sudo ufw allow from YOUR_PROMETHEUS_IP to any port 28686 proto tcp
sudo ufw enable
```

Docker skips ufw rules, so publish private ports on loopback instead: `-p 127.0.0.1:28686:28686`.

### HTTPS

Put a reverse proxy in front of `localhost:8888`. Caddy gets the certificate on its own:

```caddy
tenderduty.example.com {
    reverse_proxy localhost:8888
}
```

nginx, with certbot for the certificate. The dashboard uses a websocket, so pass the upgrade headers:

```nginx
server {
    server_name tenderduty.example.com;
    location / {
        proxy_pass http://127.0.0.1:8888;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $http_host;
    }
}
```

### Private dashboard

Close the direct port with `listen_port: 127.0.0.1:8888` in `config.yml` and restart, so only the proxy reaches it. In Docker, keep `listen_port` as is and publish `-p 127.0.0.1:8888:8888` instead.

Then add a password. Caddy, with the hash from `caddy hash-password`:

```caddy
tenderduty.example.com {
    basic_auth {
        admin $2a$14$...
    }
    reverse_proxy localhost:8888
}
```

nginx, with the file from `sudo htpasswd -c /etc/nginx/.htpasswd admin` (package `apache2-utils`), inside `location /`:

```nginx
auth_basic "tenderduty";
auth_basic_user_file /etc/nginx/.htpasswd;
```

Or skip the proxy and use an SSH tunnel, then open `http://127.0.0.1:8888`:

```shell
ssh -L 8888:127.0.0.1:8888 YOUR_USER@YOUR_MONITOR_SERVER
```
