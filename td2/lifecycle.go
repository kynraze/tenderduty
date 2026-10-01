package tenderduty

import (
	"context"
	"time"

	dash "github.com/blockpane/tenderduty/v2/td2/dashboard"
)

func (c *Config) context() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *Config) startWorker(work func()) {
	c.workers.Add(1)
	go func() { defer c.workers.Done(); work() }()
}

func (c *Config) sendStat(update *promUpdate) {
	select {
	case c.statsChan <- update:
	case <-c.context().Done():
	}
}

func (c *Config) sendUpdate(update *dash.ChainStatus) {
	select {
	case c.updateChan <- update:
	case <-c.context().Done():
	}
}

func (cc *ChainConfig) setMonitoring(active bool) {
	cc.stateMux.Lock()
	defer cc.stateMux.Unlock()
	cc.monitoring = active
	if active {
		cc.monitoringSince = time.Now()
	}
}

func (cc *ChainConfig) isMonitoring() bool {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	return cc.monitoring && time.Since(cc.monitoringSince) < time.Minute
}

func (c *Config) healthStatus() dash.HealthStatus {
	status := dash.HealthStatus{Alive: c.context().Err() == nil, Validators: len(c.Chains)}
	networks := make(map[string]bool)
	for _, chain := range c.Chains {
		networks[chain.ChainId] = true
		if chain.isMonitoring() {
			status.Monitoring++
		}
		if chain.hasNoNodes() {
			status.Unavailable++
		}
		info, _ := chain.validatorState()
		if info.SigningStale || info.ValidatorStale {
			status.Stale++
		}
	}
	status.Chains = len(networks)
	status.Ready = status.Alive && status.Validators > 0 && status.Monitoring == status.Validators
	return status
}
