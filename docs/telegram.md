# Telegram Configuration

1. Create a channel on Telegram, no need to add people yet:
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
* `/status <chain>` gives one chain in full: height, misses and how many are left before jailing, voting power and rank, RPC nodes and active alerts. The chain is its name or chain id. With `fibre_enabled` it also shows Fibre, see [fibre.md](fibre.md#status-and-the-dashboard).
* `/report` and `/report weekly` show the current daily or weekly [report](config.md#reports).
* `/mentionme` pings you in every alert sent to this chat, `/mentionme off` stops. It is saved with the state.

Commands are read-only, nothing can be changed from telegram. Only the chat in `channel` is listened to, and in a group only the user ids in `admins`. Telegram lets one program read a bot's messages, so use a bot dedicated to tenderduty.
