# Discord Configuration

* First create a channel:

![channel](img/di-channel.png)

* Then add members, or skip. Once the channel is created, right click on the channel and select "edit channel", then select "Integrations"

![edit](img/di-edit.png)
![integrations](img/di-editing.png)

* Create a webhook, give it a name, copy the URL, and save it.

![create](img/di-webhook.png)
![name](img/di-name.png)
![save](img/di-save.png)

The webhook URL is a secret: anyone with it can post to the channel. Paste it when the command waits (`=-` reads it hidden):

```shell
tenderduty config set discord.enabled=yes discord.webhook=-
```

Or in `config.yml`:

```yaml
discord:
  enabled: yes
  webhook: "https://discord.com/api/webhooks/..."
  style: embed   # or plain
```

`embed` is a block with a color down the side, `plain` an ordinary message. Set these in the shared part of `config.yml`, or per chain under `alerts.discord`.
