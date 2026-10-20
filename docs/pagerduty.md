# Pagerduty configuration

Use an Events API v2 integration key (32 characters). An OAuth token is rejected at startup.

## To generate your pagerduty integration key

- Within PagerDuty, go to Services --> Service Directory --> New Service
![services](img/pd-services.png)
![newservice](img/pd-newservice.png)
- Give your service a name, select an escalation policy, and set an alert grouping preference
![create](img/pd-create.png)
![policy](img/pd-policy.png)
- Select the PagerDuty Events API V2 Integration, hit Create Service
![integrations](img/pd-integrate.png)
- Copy the 32 character `Integration Key`
![key](img/pd-key.png)

## Configuration

Paste the key when the command waits (`=-` reads it hidden):

```shell
tenderduty config set pagerduty.enabled=yes pagerduty.api_key=-
```

Or in `config.yml`:

```yaml
pagerduty:
  enabled: yes
  api_key: "your-integration-key"
```

Set these in the shared part of `config.yml`, or per chain under `alerts.pagerduty`.
