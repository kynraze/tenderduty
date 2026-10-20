# Gotify configuration

[Gotify](https://gotify.net) is a self-hosted push server with a phone app. Tenderduty posts alerts and notices to it. Reports are not sent.

1. Run the server, here on localhost behind Caddy or nginx for HTTPS:

   ```shell
   read -s GP   # admin password
   docker run -d --name gotify --restart unless-stopped -p 127.0.0.1:8090:80 \
     -v /var/lib/gotify:/app/data -e GOTIFY_DEFAULTUSER_PASS="$GP" gotify/server
   ```

2. In the Gotify web UI, log in as `admin`, open Apps, create an application and copy its token.
3. Connect the phone app to the HTTPS address.

Then set it, pasting the token when the command waits (`=-` reads it hidden):

```shell
tenderduty config set gotify.enabled=yes gotify.url=http://127.0.0.1:8090 gotify.token=-
```

Or in `config.yml`:

```yaml
gotify:
  enabled: yes
  url: "http://127.0.0.1:8090"   # on the same server; else https://gotify.example.com
  token: "your-app-token"
```

Set these in the shared part of `config.yml`, or per chain under `alerts.gotify`.

Priority follows severity: info 2, warning 5, error 7, critical 8. An [escalated](config.md#escalation) alert is 10, a recovery 2. The title is the alert heading, the body is the plain text of the alert.

The token goes in the `X-Gotify-Key` header, never in the URL. A failed send logs the status code without the token.
