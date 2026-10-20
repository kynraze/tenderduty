package tenderduty

import "fmt"

// nodeDisplay names nodes in alerts; alarm keys keep the URL.
func (c *Config) nodeDisplay(chain, nodeURL string) string {
	address := c.settings().AlertNodeAddress
	showAddress := address == nil || *address
	c.chainsMux.RLock()
	cc := c.Chains[chain]
	c.chainsMux.RUnlock()
	if cc != nil {
		own := 0
		for i, node := range cc.Nodes {
			if node == nil {
				continue
			}
			if node.AlertIfDown {
				own++
			}
			if node.Url != nodeURL {
				continue
			}
			label := node.serverLabel()
			if label == "" {
				label = c.serverName(nodeURL)
			}
			if showAddress {
				if label != "" {
					return label + " · " + nodeEndpoint(nodeURL)
				}
				return nodeEndpoint(nodeURL)
			}
			if label != "" {
				return label
			}
			if node.AlertIfDown {
				return fmt.Sprintf("own node %d", own)
			}
			return fmt.Sprintf("node %d", i+1)
		}
	}
	if label := c.serverName(nodeURL); label != "" {
		if showAddress {
			return label + " · " + nodeEndpoint(nodeURL)
		}
		return label
	}
	if showAddress {
		return nodeEndpoint(nodeURL)
	}
	return "node"
}
