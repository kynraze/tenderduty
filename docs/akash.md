# Example Akash Deployment

[Akash](https://akash.network/) can run tenderduty in a small container.

Steps:

* First, you will need to create, configure, and encrypt your configuration file. Akash has no `chains.d`, so put the chains under `chains:` in `config.yml`. See the [Remote](remote.md) documentation for both. *It's a good idea to test the configuration with a local Docker container before deploying.*
* You will need to create a key and acquire some [$AKT](https://docs.akash.network/tokens-and-wallets) tokens.
* The easiest way to deploy is using the [Akash desktop app](https://docs.akash.network/guides/deploy)

In the dashboard click on 'Create Deployment', then select 'Empty'
![akt dashboard](img/akt-dash.png)
![what to deploy](img/akt-what.png)

Paste in the configuration and click 'Create Deployment', you will be prompted to sign a transaction.

![create deployment](img/akt-create.png)

Below is an example config, don't forget to update the PASSWORD and CONFIG variables. 

*Tip: using the raw link to a private Github gist is a handy way to host the config, but will require updating the CONFIG variable and re-deploying each time the config file changes.*

```yaml
---
version: "2.0"

services:
  td2:
    image: ghcr.io/kynraze/tenderduty:latest
    env:
      - PASSWORD=<key-to-decrypt>
      - CONFIG=https://host.name/config.yml.asc
    expose:
      - port: 8888
        as: 80
        to:
          - global: true

profiles:
  compute:
    td2:
      resources:
        cpu:
          units: 1
        memory:
          size: 256Mi
        storage:
          size: 100Mi
  placement:
    dcloud:
      attributes:
        host: akash
      signedBy:
        anyOf:
          - akash1365yvmc4s7awdyj3n2sav7xfx76adc6dnmlx63
      pricing:
        td2:
          denom: uakt
          amount: 1000

deployment:
  td2:
    dcloud:
      profile: td2
      count: 1

```

The `expose` block makes the dashboard public without auth. Set `hide_logs: yes` in `config.yml`, or remove `expose` if you don't need the dashboard.

If the transaction is successful, you will get a few bids. Select one and press 'Accept Bid'.

![accept bid](img/akt-bids.png)

After the bid is accepted, the deployments screen shows the event log. Once it shows deployed, tenderduty is running.

![deployed](img/akt-deployed.png)

You can get the hostname for the deployment on the deployments screen by selecting your deployment.

![deployments](img/akt-deployments.png)

![td dash on akash](img/akt-running.png)