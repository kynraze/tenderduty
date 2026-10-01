package tenderduty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	dash "github.com/blockpane/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

// newRpc sets up the rpc client used for monitoring. It will try nodes in order until a working node is found.
// it will also get some initial info on the validator's status.
func (cc *ChainConfig) newRpc() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var anyWorking bool // if healthchecks are running, we will skip to the first known good node.
	for _, endpoint := range cc.Nodes {
		anyWorking = anyWorking || !endpoint.snapshot().down
	}
	// grab the first working endpoint
	tryUrl := func(u string) (msg string, down, syncing bool) {
		_, err := url.Parse(u)
		if err != nil {
			msg = fmt.Sprintf("❌ could not parse url %s: (%s) %s", cc.name, u, err)
			l(msg)
			down = true
			return
		}
		client, err := rpchttp.New(u, "/websocket")
		if err != nil {
			msg = fmt.Sprintf("❌ could not connect client for %s: (%s) %s", cc.name, u, err)
			l(msg)
			down = true
			return
		}
		status, err := client.Status(ctx)
		if err != nil {
			msg = fmt.Sprintf("❌ could not get status for %s: (%s) %s", cc.name, u, err)
			down = true
			l(msg)
			return
		}
		if status.NodeInfo.Network != cc.ChainId {
			msg = fmt.Sprintf("chain id %s on %s does not match, expected %s, skipping", status.NodeInfo.Network, u, cc.ChainId)
			down = true
			l(msg)
			return
		}
		if status.SyncInfo.CatchingUp {
			msg = fmt.Sprint("🐢 node is not synced, skipping ", u)
			syncing = true
			down = true
			l(msg)
			return
		}
		cc.setNoNodes(false)
		cc.setClient(client)
		return
	}
	for _, endpoint := range cc.Nodes {
		if anyWorking && endpoint.snapshot().down {
			continue
		}
		if msg, failed, syncing := tryUrl(endpoint.Url); failed {
			endpoint.markDown(msg, syncing)
			continue
		}
		return nil
	}
	if cc.PublicFallback {
		if u, ok := getRegistryUrl(cc.ChainId); ok {
			node := guessPublicEndpoint(u)
			l(cc.ChainId, "⛑ attemtping to use public fallback node", node)
			if _, kk, _ := tryUrl(node); !kk {
				l(cc.ChainId, "⛑ connected to public endpoint", node)
				return nil
			}
		} else {
			l("could not find a public endpoint for", cc.ChainId)
		}
	}
	cc.setNoNodes(true)
	cc.lastError = "no usable RPC endpoints available for " + cc.ChainId
	if td.EnableDash {
		info, _ := cc.validatorState()
		td.updateChan <- &dash.ChainStatus{
			MsgType:      "status",
			Name:         cc.name,
			ChainId:      cc.ChainId,
			Moniker:      info.Moniker,
			Bonded:       info.Bonded,
			Jailed:       info.Jailed,
			Tombstoned:   info.Tombstoned,
			Missed:       info.Missed,
			Window:       info.Window,
			Nodes:        len(cc.Nodes),
			HealthyNodes: 0,
			ActiveAlerts: 1,
			Height:       0,
			LastError:    cc.lastError,
			Blocks:       cc.blocksSnapshot(),
		}
	}
	return errors.New("no usable endpoints available for " + cc.ChainId)
}

func (cc *ChainConfig) monitorHealth(ctx context.Context, chainName string) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-tick.C:
			var err error
			for _, node := range cc.Nodes {
				go func(node *NodeConfig) {
					alert := func(msg string) {
						status := node.markDown(fmt.Sprintf("%-12s node %s is %s", chainName, node.Url, msg), msg == "not synced")
						if !node.AlertIfDown {
							// even if we aren't alerting, we want to display the status in the dashboard.
							return
						}
						if td.Prom {
							td.statsChan <- cc.mkUpdate(metricNodeDownSeconds, time.Since(status.downSince).Seconds(), node.Url)
						}
						l("⚠️ " + status.lastMsg)
					}
					c, e := rpchttp.New(node.Url, "/websocket")
					if e != nil {
						alert(e.Error())
						return
					}
					cwt, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					status, e := c.Status(cwt)
					cancel()
					if e != nil {
						alert("down")
						return
					}
					if status.NodeInfo.Network != cc.ChainId {
						alert("on the wrong network")
						return
					}
					if status.SyncInfo.CatchingUp {
						alert("not synced")
						return
					}

					// node's OK, clear the note
					td.statsChan <- cc.mkUpdate(metricNodeDownSeconds, 0, node.Url)
					node.markHealthy()
					cc.setNoNodes(false)
					l(fmt.Sprintf("🟢 %-12s node %s is healthy", chainName, node.Url))
				}(node)
			}

			err = cc.GetValInfo(false)
			if err != nil {
				l("❓ refreshing signing info for", cc.ValAddress, err)
			}
		}
	}
}

func (c *Config) pingHealthcheck() {
	if !c.Healthcheck.Enabled {
		return
	}

	ticker := time.NewTicker(c.Healthcheck.PingRate * time.Second)

	go func() {
		defer ticker.Stop()
		client := &http.Client{Timeout: 10 * time.Second}
		for {
			select {
			case <-ticker.C:
				response, err := client.Get(c.Healthcheck.PingURL)
				if err != nil {
					l(fmt.Sprintf("❌ Failed to ping healthcheck URL: %s", err.Error()))
				} else {
					_ = response.Body.Close()
					l(fmt.Sprintf("🏓 Successfully pinged healthcheck URL: %s", c.Healthcheck.PingURL))
				}
			case <-c.ctx.Done():
				return
			}
		}
	}()
}

// endpointRex matches the first a tag's hostname and port if present.
var endpointRex = regexp.MustCompile(`//([^/:]+)(:\d+)?`)

// guessPublicEndpoint attempts to deal with a shortcoming in the tendermint RPC client that doesn't allow path prefixes.
// The cosmos.directory requires them. This is a workaround to get the actual URL for the server behind their proxy.
// The RPC base URL will return links endpoints, and we can parse this to guess the original URL.
func guessPublicEndpoint(u string) string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(u + "/")
	if err != nil {
		return u
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return u
	}
	matches := endpointRex.FindStringSubmatch(string(b))
	if len(matches) < 2 {
		// didn't work
		return u
	}
	proto := "https://"
	port := ":443"
	// will be 3 elements if there is a port no port means listening on https
	if len(matches) == 3 && matches[2] != "" && matches[2] != ":443" {
		proto = "http://"
		port = matches[2]
	}
	return proto + matches[1] + port
}
