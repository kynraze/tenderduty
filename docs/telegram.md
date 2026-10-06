# Telegram configuration

1. Create a channel on Telegram. You can add people later:
   https://www.alphr.com/telegram-how-to-create-supergroup/
2. Create a bot with @botfather and save its API key:
   https://riptutorial.com/telegram-bot/example/25075/create-a-bot-with-the-botfather
3. Add the bot to the channel and make it an admin so it can post:
   https://www.alphr.com/add-bot-telegram/ and https://www.alphr.com/add-admin-telegram/
4. Get the channel id and add both to `config.yml`:
   https://stackoverflow.com/questions/45414021/get-telegram-channel-group-id
   A supergroup or channel id like `1068773197` is `-1001068773197` for bots (with the `-100` prefix).

```yaml
telegram:
  enabled: yes
  api_key: "your-bot-token"
  channel: "-1001068773197"
```

Set these in the shared part of `config.yml`, or per chain under `alerts.telegram`.

## Commands

With `commands: yes` in the telegram settings, tenderduty answers these in the configured chat:

* `/status` gives a line per chain, problems first.
* `/status <chain>` gives one chain in full: height, misses and how many are left before jailing, voting power and rank, RPC nodes and active alerts. Nodes with `alert_if_down` show their celestia-app version; `hide_logs` hides the node lines. The chain is its name or chain id. With `fibre_enabled` it also shows Fibre with the 24 hour endorsement line, see [fibre.md](fibre.md#status-and-the-dashboard).
* `/report` and `/report weekly` show the current daily or weekly [report](config.md#reports).
* `/mentionme` pings you in every alert sent to this chat. `/mentionme off` stops these pings. The preference is saved with the state.

Commands show monitoring data and let users manage their mentions. Tenderduty listens only to the chat in `channel`. Anyone who can post in a channel can use commands; in a group, only the user ids in `admins` can. Other users are logged with their id. Telegram lets one program read a bot's messages, so use a bot dedicated to tenderduty.
