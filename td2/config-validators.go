package tenderduty

import "fmt"

// Each validator tries its own nodes before shared chain nodes.
func expandValidators(c *Config) ([]string, error) {
	warnings := make([]string, 0)
	for chainName, chain := range c.Chains {
		if len(chain.Validators) == 0 {
			continue
		}
		if chain.ValAddress != "" {
			return nil, fmt.Errorf("%s cannot set both valoper_address and validators", chainName)
		}
		shared := make([]*NodeConfig, 0, len(chain.Nodes))
		for _, node := range chain.Nodes {
			if node == nil {
				return nil, fmt.Errorf("%s has an empty RPC node", chainName)
			}
			// shared nodes would page once per validator
			if node.AlertIfDown {
				warnings = append(warnings, fmt.Sprintf("warn: %20s alert_if_down is ignored on the shared node %s, set it on a validator's own node", chainName, node.Url))
			}
			shared = append(shared, &NodeConfig{Url: node.Url, Server: node.Server})
		}
		delete(c.Chains, chainName)
		for _, validator := range chain.Validators {
			if validator.Name == "" || validator.ValAddress == "" {
				return nil, fmt.Errorf("%s validators require name and valoper_address", chainName)
			}
			name := chainName + " / " + validator.Name
			if c.Chains[name] != nil {
				return nil, fmt.Errorf("duplicate validator name %s", name)
			}
			copied := &ChainConfig{
				name: name, ChainId: chain.ChainId, ValAddress: validator.ValAddress,
				ValconsOverride: validator.ValconsOverride, ExtraInfo: chain.ExtraInfo,
				Alerts: chain.Alerts, PublicFallback: chain.PublicFallback, Testnet: chain.Testnet, Bridge: chain.Bridge,
				Nodes: make([]*NodeConfig, 0, len(validator.Nodes)+len(shared)),
			}
			for _, node := range validator.Nodes {
				if node == nil {
					return nil, fmt.Errorf("%s has an empty RPC node", name)
				}
				copied.Nodes = append(copied.Nodes, &NodeConfig{Url: node.Url, AlertIfDown: node.AlertIfDown, Server: node.Server})
			}
			for _, node := range shared {
				copied.Nodes = append(copied.Nodes, &NodeConfig{Url: node.Url, Server: node.Server})
			}
			if len(copied.Nodes) == 0 && !copied.PublicFallback {
				return nil, fmt.Errorf("%s has no RPC nodes, give it one or set public_fallback", name)
			}
			c.Chains[name] = copied
		}
	}
	return warnings, nil
}
