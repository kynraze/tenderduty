package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	dashboardLogoLookups sync.Map
	dashboardLogos       sync.Map
	registryLogoPrefix   = "https://raw.githubusercontent.com/cosmos/chain-registry/"
)

func (cc *ChainConfig) lookupDashboardLogo(ctx context.Context) {
	if cc.ChainId == "" {
		return
	}
	if _, started := dashboardLogoLookups.LoadOrStore(cc.ChainId, true); started {
		return
	}
	for {
		lookup, cancel := context.WithTimeout(ctx, 10*time.Second)
		chain, err := fetchRegistryChain(lookup, cc.ChainId)
		if err == nil {
			if logo := fetchDashboardLogo(lookup, chain.LogoURIs.PNG); len(logo) > 0 {
				dashboardLogos.Store(cc.ChainId, logo)
			}
		}
		cancel()
		if !registryDown(err) {
			return
		}
		// the registry is down, not missing the chain
		if !waitContext(ctx, 30*time.Minute) {
			dashboardLogoLookups.Delete(cc.ChainId)
			return
		}
	}
}

func registryDown(err error) bool {
	var status registryStatusError
	if errors.As(err, &status) {
		return int(status) >= 500
	}
	return err != nil && !errors.Is(err, errNotInRegistry)
}

func fetchDashboardLogo(ctx context.Context, address string) []byte {
	if !strings.HasPrefix(address, registryLogoPrefix) {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || !strings.HasPrefix(req.URL.String(), registryLogoPrefix) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	const maxSize = 256 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil || len(data) > maxSize {
		return nil
	}
	// registry logos run to ~2000px; the size cap keeps them small
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" || config.Width > 4096 || config.Height > 4096 {
		return nil
	}
	return data
}

func cachedDashboardLogo(chainID string) []byte {
	logo, _ := dashboardLogos.Load(chainID)
	data, _ := logo.([]byte)
	return data
}

func (c *Config) dashboardLogo(name string) []byte {
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	if cc := c.Chains[name]; cc != nil {
		return cachedDashboardLogo(cc.ChainId)
	}
	return nil
}
