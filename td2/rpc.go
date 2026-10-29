package tenderduty

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

var rpcEndpointTimeout = 10 * time.Second

const nodeStuckAfter = 2 * time.Minute

func nodeLag(headHeight int64, headSeen time.Time, nodeHeight int64, nodeBlock time.Time, now time.Time) (behind int64, lag time.Duration, stuck bool) {
	if headHeight <= nodeHeight {
		return
	}
	behind = headHeight - nodeHeight
	lag = now.Sub(nodeBlock)
	if lag < 0 {
		lag = 0
	}
	stuck = headHeight > 0 && !headSeen.IsZero() && now.Sub(headSeen) <= nodeStuckAfter && lag > nodeStuckAfter
	return
}

// newRpc sets up the rpc client used for monitoring. It will try nodes in order until a working node is found.
// it will also get some initial info on the validator's status.
func (cc *ChainConfig) newRpc() error {
	// grab the first working endpoint
	tryUrl := func(u string) (msg string, down, syncing bool) {
		ctx, cancel := context.WithTimeout(cc.context(), rpcEndpointTimeout)
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
		if !websocketWorks(cc.context(), u) {
			msg = fmt.Sprintf("❌ %s refuses websocket connections, tenderduty needs /websocket", u)
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
		if cc.context().Err() != nil {
			return cc.context().Err()
		}
		if msg, failed, syncing := tryUrl(endpoint.Url); failed {
			cc.markNodeDown(endpoint, msg, syncing)
			continue
		}
		endpoint.markHealthy()
		return nil
	}
	if cc.publicFallbackEnabled() {
		if node, ok := cc.publicFallback(tryUrl); ok {
			l(cc.ChainId, "⛑ connected to public endpoint", node)
			return nil
		}
		l(cc.ChainId, "⛑ no public endpoint with a working websocket")
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
					stuckDetail := ""
					alert := func(msg string) {
						status := cc.markNodeDown(node, fmt.Sprintf("%-12s node %s is %s", chainName, node.Url, msg), msg == "not synced", stuckDetail)
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
					cc.refreshNodeBinary(ctx, node, c, time.Now())
					if e != nil {
						alert("down")
						return
					}
					if status.NodeInfo.Network != cc.ChainId {
						alert("on the wrong network")
						return
					}
					cc.stateMux.RLock()
					headHeight, headSeen := cc.lastBlockNum, cc.lastBlockTime
					cc.stateMux.RUnlock()
					behind, lag, stuck := nodeLag(headHeight, headSeen, status.SyncInfo.LatestBlockHeight, status.SyncInfo.LatestBlockTime, time.Now())
					if td.Prom {
						td.sendStat(cc.mkUpdate(metricNodeLagSeconds, lag.Seconds(), node.Url))
					}
					if status.SyncInfo.CatchingUp {
						alert("not synced")
						return
					}

					if !websocketWorks(ctx, node.Url) {
						alert("refusing websocket connections")
						return
					}
					if stuck {
						stuckDetail = fmt.Sprintf("stuck at height %s, %s blocks behind", thousands(status.SyncInfo.LatestBlockHeight), thousands(behind))
						alert(stuckDetail)
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
			if client := cc.clientSnapshot(); client != nil && (cc.alertSettings().GovAlerts || cc.alertSettings().UpgradeAlerts || cc.alertSettings().FibreAlerts || td.reportsEnabled()) {
				ctx, cancel := context.WithTimeout(cc.context(), 30*time.Second)
				cc.refreshProposals(ctx, client)
				if cc.alertSettings().UpgradeAlerts || td.reportsEnabled() {
					cc.refreshUpgrade(ctx, client)
				}
				if cc.alertSettings().UpgradeAlerts || td.reportsEnabled() {
					cc.refreshSignal(ctx, client)
				}
				if cc.alertSettings().FibreAlerts {
					cc.refreshFibre(ctx, client)
				}
				cancel()
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
