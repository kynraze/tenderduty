# Loading Remote Configurations

Tenderduty can load a configuration file over HTTP/HTTPS, including in containers that receive the decryption secret through an environment variable. The file must be encrypted with a password. Tenderduty refuses plaintext remote configurations.

The remote file is not polled. Fetch changes with `tenderduty reload` or a restart.

Only `config.yml` is fetched. Where there is no local `chains.d`, as in a container, put the chains under `chains:` before encrypting:

```yaml
# config.yml
chains:
  Osmosis:
    chain_id: osmosis-1
    valoper_address: osmovaloper1xxxxxxx...
    nodes:
      - url: http://YOUR_OSMOSIS_NODE:26657
        alert_if_down: yes
```

The [encryption](../td2/encryption.go) is AES-256 with an authenticated SHA-256 HMAC, and keys derived with Argon2id. Use a good password.

## Generating the Encrypted Config

Run `tenderduty -encrypt` next to `config.yml`. It prompts for a password (at least 8 characters, not a common one) and writes base64 to `config.yml.asc`. Use `-encrypted-config` for another output file.

```
$ ./tenderduty -encrypt
Please enter the encryption password:
2022/07/04 15:32:08 wrote 8856 bytes to encrypted file config.yml.asc
```

## Restoring the Encrypted Config

`-decrypt` reverses it. It writes to `config.yml` by default, so use `-f` to keep an existing config.

```
$ ./tenderduty -decrypt -f decrypted.yml -encrypted-config config.yml.asc
Please enter the encryption password:
2022/07/04 15:36:57 wrote 6549 bytes to decrypted file decrypted.yml
```

## Using a Remotely Hosted Config File

If the config file setting starts with `http://` or `https://`, tenderduty fetches it from there. Set it with `-f` or the `CONFIG` environment variable.

A password is required, from the `-password` flag or the `PASSWORD` environment variable. Prefer the variable: it is cleared once read, while `-password` is visible to other users in the process list.
