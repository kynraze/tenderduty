package tenderduty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

// newRpc sets up the rpc client used for monitoring. It will try nodes in order until a working node is found.
// it will also get some initial info on the validator's status.
func (cc *ChainConfig) newRpc() error {
	// grab the first working endpoint
	tryUrl := func(u string) (msg string, down, syncing bool) {
		ctx, cancel := context.WithTimeout(td.context(), 10*time.Second)
		defer cancel()
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
	// try nodes in the configured order, but nodes known to be down go after the healthy ones, and the endpoint that
	// just failed to deliver blocks goes last.
	cc.stateMux.RLock()
	skip := cc.rpcSkip
	cc.stateMux.RUnlock()
	ordered := make([]*NodeConfig, 0, len(cc.Nodes))
	var down, last []*NodeConfig
	for _, endpoint := range cc.Nodes {
		switch {
		case endpoint.Url == skip:
			last = append(last, endpoint)
		case endpoint.snapshot().down:
			down = append(down, endpoint)
		default:
			ordered = append(ordered, endpoint)
		}
	}
	ordered = append(append(ordered, down...), last...)
	for _, endpoint := range ordered {
		if td.context().Err() != nil {
			return td.context().Err()
		}
		if msg, failed, syncing := tryUrl(endpoint.Url); failed {
			endpoint.markDown(msg, syncing)
			continue
		}
		endpoint.markHealthy()
		return nil
	}
	if cc.PublicFallback {
		if u, ok := getRegistryUrl(cc.ChainId); ok {
			node := guessPublicEndpoint(td.context(), u)
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
	if td.EnableDash {
		td.sendUpdate(cc.dashboardStatus())
	}
	return errors.New("no usable endpoints available for " + cc.ChainId)
}

// setRpcSkip marks an endpoint that failed so newRpc tries it last, once one works we go back to the configured order.
func (cc *ChainConfig) setRpcSkip(endpoint string, failed bool) {
	cc.stateMux.Lock()
	defer cc.stateMux.Unlock()
	if failed {
		cc.rpcSkip = endpoint
	} else {
		cc.rpcSkip = ""
	}
}

func (cc *ChainConfig) monitorHealth(ctx context.Context, chainName string) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var checks sync.WaitGroup
	defer checks.Wait()

	for {
		select {
		case <-ctx.Done():
			return

		case <-tick.C:
			var err error
			for _, node := range cc.Nodes {
				checks.Add(1)
				go func(node *NodeConfig) {
					defer checks.Done()
					alert := func(msg string) {
						status := node.markDown(fmt.Sprintf("%-12s node %s is %s", chainName, node.Url, msg), msg == "not synced")
						if !node.AlertIfDown {
							// even if we aren't alerting, we want to display the status in the dashboard.
							return
						}
						if td.Prom {
							td.sendStat(cc.mkUpdate(metricNodeDownSeconds, time.Since(status.downSince).Seconds(), node.Url))
						}
						l("⚠️ " + status.lastMsg)
					}
					c, e := rpchttp.New(node.Url, "/websocket")
					if e != nil {
						alert(e.Error())
						return
					}
					cwt, cancel := context.WithTimeout(ctx, 10*time.Second)
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
					if td.Prom {
						td.sendStat(cc.mkUpdate(metricNodeDownSeconds, 0, node.Url))
					}
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

	ticker := time.NewTicker(time.Duration(c.Healthcheck.PingRate) * time.Second)

	c.startWorker(func() {
		defer ticker.Stop()
		client := &http.Client{Timeout: 10 * time.Second}
		for {
			select {
			case <-ticker.C:
				err := pingURL(c.ctx, client, c.Healthcheck.PingURL)
				if err != nil {
					l(fmt.Sprintf("❌ Failed to ping healthcheck URL: %s", err.Error()))
				} else {
					status := c.healthStatus()
					l(fmt.Sprintf("🏓 Process heartbeat accepted; monitoring %d/%d validators", status.Monitoring, status.Validators))
				}
			case <-c.ctx.Done():
				return
			}
		}
	})
}

func pingURL(ctx context.Context, client *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("healthcheck returned status %d", response.StatusCode)
	}
	return nil
}

// endpointRex matches the first a tag's hostname and port if present.
var endpointRex = regexp.MustCompile(`//([^/:]+)(:\d+)?`)

// guessPublicEndpoint attempts to deal with a shortcoming in the tendermint RPC client that doesn't allow path prefixes.
// The cosmos.directory requires them. This is a workaround to get the actual URL for the server behind their proxy.
// The RPC base URL will return links endpoints, and we can parse this to guess the original URL.
func guessPublicEndpoint(ctx context.Context, u string) string {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"/", nil)
	if err != nil {
		return u
	}
	resp, err := client.Do(req)
	if err != nil {
		return u
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
