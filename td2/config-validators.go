package tenderduty

import "fmt"

// expandValidators lets validators on the same chain share one configuration.
func expandValidators(c *Config) error {
	for chainName, chain := range c.Chains {
		if len(chain.Validators) == 0 {
			continue
		}
		if chain.ValAddress != "" {
			return fmt.Errorf("%s cannot set both valoper_address and validators", chainName)
		}
		delete(c.Chains, chainName)
		for _, validator := range chain.Validators {
			if validator.Name == "" || validator.ValAddress == "" {
				return fmt.Errorf("%s validators require name and valoper_address", chainName)
			}
			name := chainName + " / " + validator.Name
			if c.Chains[name] != nil {
				return fmt.Errorf("duplicate validator name %s", name)
			}
			copy := &ChainConfig{
				name: name, ChainId: chain.ChainId, ValAddress: validator.ValAddress,
				ValconsOverride: validator.ValconsOverride, ExtraInfo: chain.ExtraInfo,
				Alerts: chain.Alerts, PublicFallback: chain.PublicFallback,
				Nodes: make([]*NodeConfig, len(chain.Nodes)),
			}
			for i, node := range chain.Nodes {
				if node == nil {
					return fmt.Errorf("%s has an empty RPC node", chainName)
				}
				copy.Nodes[i] = &NodeConfig{Url: node.Url, AlertIfDown: node.AlertIfDown}
			}
			c.Chains[name] = copy
		}
	}
	return nil
}
