package tenderduty

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

func metricsEndpoint(m FibreMetricsConfig, remote, override string) (string, error) {
	if override != "" {
		u, err := url.Parse(override)
		if err != nil || !validHTTPURL(override) || u.User != nil {
			return "", fmt.Errorf("endpoint must be an http:// or https:// URL")
		}
		return strings.TrimRight(override, "/"), nil
	}
	host, port, err := net.SplitHostPort(m.Listen)
	if err != nil {
		host, port = "", "4318"
	}
	ip := net.ParseIP(host)
	if host == "" || ip != nil && ip.IsUnspecified() {
		host = "<this server's IP>"
		if remote != "" {
			if u, err := url.Parse(remote); err == nil && u.Hostname() != "" {
				remote = u.Hostname()
			} else if h, _, err := net.SplitHostPort(remote); err == nil {
				remote = h
			}
			conn, err := net.DialTimeout("udp", net.JoinHostPort(remote, port), time.Second)
			if err == nil {
				host = conn.LocalAddr().(*net.UDPAddr).IP.String()
				conn.Close()
			}
		}
	}
	scheme := "http"
	if m.TLSCert != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

func printMetricsSetup(m FibreMetricsConfig, endpoint, chainID string, bridge bool, output io.Writer) {
	if m.Listen == "" {
		fmt.Fprintln(output, "Run first: tenderduty config set fibre_metrics.listen=<ip>:4318 fibre_metrics.token=generate")
	}
	if m.Token != "" {
		fmt.Fprintln(output, "token: fibre_metrics.token in config.yml (config show hides it)")
	}
	if bridge {
		scheme, address, _ := strings.Cut(endpoint, "://")
		address, _, _ = strings.Cut(address, "/")
		fmt.Fprintf(output, "celestia bridge start --metrics --metrics.endpoint %s --metrics.tls=%t\n", address, scheme == "https")
		if m.Token != "" {
			fmt.Fprintln(output, `Environment="OTEL_EXPORTER_OTLP_METRICS_HEADERS=Authorization=Bearer <token>"`)
		}
	} else {
		if m.Token != "" {
			fmt.Fprintln(output, `OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer <token>" \`)
		}
		fmt.Fprintln(output, "OTEL_RESOURCE_ATTRIBUTES="+strconv.Quote("service.namespace="+chainID)+` \`)
		fmt.Fprintf(output, "  fibre start --otel-endpoint %s\n", endpoint)
	}
}

func commaHeight(height string) string {
	for i := len(height) - 3; i > 0; i -= 3 {
		height = height[:i] + "," + height[i:]
	}
	return height
}

func setupBridge(ctx context.Context, options ChainCommandOptions, name, address, tokenFile, alert, override string, force bool, output io.Writer) error {
	name, filename, err := standaloneChain(options, name)
	if err != nil {
		return err
	}
	u, err := url.Parse(address)
	if err != nil || !validHTTPURL(address) || u.User != nil {
		return fmt.Errorf("bridge URL must be http:// or https://")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	var cc ChainConfig
	if err := decodeConfig(data, &cc); err != nil {
		return err
	}
	cc.Bridge.URL = address
	if tokenFile != "" {
		cc.Bridge.TokenFile = tokenFile
	}
	if cc.Bridge.TokenFile != "" {
		token, err := os.ReadFile(cc.Bridge.TokenFile)
		if err != nil {
			return fmt.Errorf("cannot read bridge token_file: %w", err)
		}
		cc.Bridge.token = strings.TrimSpace(string(token))
		if strings.ContainsAny(cc.Bridge.token, "\r\n") {
			return fmt.Errorf("invalid bridge token_file")
		}
	}
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err != nil {
		return err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	endpoint, err := metricsEndpoint(c.FibreMetrics, address, override)
	if err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var head struct {
		Header struct {
			Height  string `json:"height"`
			ChainID string `json:"chain_id"`
		} `json:"header"`
	}
	var stats struct {
		Sampled *int64 `json:"head_of_sampled_chain"`
	}
	rejected, reason := cc.bridgeRPC(probe, "header.LocalHead", &head)
	if reason == "" {
		rejected, reason = cc.bridgeRPC(probe, "das.SamplingStats", &stats)
	}
	if reason == "" {
		h, err := strconv.ParseInt(head.Header.Height, 10, 64)
		if err != nil || h < 0 || stats.Sampled == nil || *stats.Sampled < 0 {
			reason = "invalid RPC result"
		}
	}
	if reason == "" && head.Header.ChainID != "" && cc.ChainId != "" && head.Header.ChainID != cc.ChainId {
		reason = fmt.Sprintf("network mismatch: bridge reports %s, chain_id is %s", head.Header.ChainID, cc.ChainId)
	}
	if reason != "" {
		if rejected {
			fmt.Fprintln(output, "✗ token rejected (401); create one with: celestia bridge auth read --p2p.network <network>")
		} else {
			fmt.Fprintf(output, "✗ unreachable: %s\n", reason)
		}
		if strings.Contains(reason, "connection") || strings.Contains(reason, "refused") || strings.Contains(reason, "timed out") {
			port := u.Port()
			if port == "" {
				port = "80"
				if u.Scheme == "https" {
					port = "443"
				}
			}
			fmt.Fprintf(output, "bridge started with --rpc.addr 0.0.0.0? port %s open to this server?\n", port)
		}
		if !force {
			return fmt.Errorf("bridge check failed; nothing saved (use --force to save anyway)")
		}
	} else {
		token := "no token"
		if cc.Bridge.token != "" {
			token = "token OK"
		}
		fmt.Fprintf(output, "✓ bridge reachable · head %s · sampled %s · %s\n", commaHeight(head.Header.Height), commaHeight(strconv.FormatInt(*stats.Sampled, 10)), token)
		validateConfig(c)
		for key, chain := range c.Chains {
			if key == name || strings.HasPrefix(key, name+" / ") {
				h, _ := strconv.ParseInt(head.Header.Height, 10, 64)
				if lag := h - *stats.Sampled; lag > chain.Alerts.BridgeLagBlocks {
					fmt.Fprintf(output, "\u26a0 sampling is %s blocks behind head; it should catch up within minutes after a restart, else check the bridge\n", commaHeight(strconv.FormatInt(lag, 10)))
				}
				break
			}
		}
	}
	if bridgeNotCelestia(ctx, &cc) {
		fmt.Fprintf(output, "\u26a0 %s doesn't look like Celestia; bridge alerts only make sense for a Celestia network\n", name)
	}
	settings := []string{"bridge.url=" + address}
	if tokenFile != "" {
		settings = append(settings, "bridge.token_file="+tokenFile)
	}
	if alert != "" {
		settings = append(settings, "alerts.bridge_enabled="+alert)
	}
	changed, err := setChains(options, append([]string{name}, settings...), false, output)
	if err != nil {
		return err
	}
	if changed {
		reloadAfterEdit(options.StateFile, output, options.reload)
	}
	printMetricsSetup(c.FibreMetrics, endpoint, cc.ChainId, true, output)
	return nil
}

// Only a definite missing module warrants a warning; transport failures say nothing.
func bridgeNotCelestia(ctx context.Context, cc *ChainConfig) bool {
	if len(cc.Nodes) == 0 {
		return false
	}
	client, err := rpchttp.New(cc.Nodes[0].Url, "/websocket")
	if err != nil {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Version 1 avoids an extra ABCIInfo query; only a missing route matters.
	_, err = abciQuery(probe, client, "/celestia.signal.v1.Query/VersionTally", []byte{8, 1})
	return moduleMissing(err)
}

func setupFibre(ctx context.Context, options ChainCommandOptions, name, instance, override string, off, hasInstance bool, output io.Writer) error {
	name, _, err := standaloneChain(options, name)
	if err != nil {
		return err
	}
	if off {
		changed, err := setChains(options, []string{name, "alerts.fibre_enabled=no"}, false, output)
		if err == nil && changed {
			reloadAfterEdit(options.StateFile, output, options.reload)
		}
		return err
	}
	c, err := loadConfigQuiet(options.ConfigFile, options.Directory, options.Password)
	if err != nil {
		return err
	}
	defer c.cancel()
	defer c.deliveryCancel()
	remote, chainID := "", ""
	queried := false
	for key, cc := range c.Chains {
		if key != name && !strings.HasPrefix(key, name+" / ") {
			continue
		}
		chainID = cc.ChainId
		cc.Alerts.FibreAlerts = true
		if hasInstance {
			cc.Alerts.FibreInstance = instance
		}
		if cc.fibre.Host != "" {
			remote = cc.fibre.Host
		}
		if cc.fibreUnsupported {
			fmt.Fprintln(output, "✗ chain has no x/valaddr support")
		}
		if !queried && remote == "" && strings.Contains(cc.ValconsOverride, "valcons1") && len(cc.Nodes) > 0 {
			queried = true
			client, err := rpchttp.New(cc.Nodes[0].Url, "/websocket")
			if err == nil {
				probe, cancel := context.WithTimeout(ctx, 10*time.Second)
				remote, _, err = getFibreProvider(probe, client, cc.ValconsOverride)
				cancel()
				if moduleMissing(err) {
					cc.fibreUnsupported = true
					fmt.Fprintln(output, "✗ chain has no x/valaddr support")
				}
			}
		}
	}
	endpoint, err := metricsEndpoint(c.FibreMetrics, remote, override)
	if err != nil {
		return err
	}
	m := c.FibreMetrics
	if m.Listen == "" {
		fmt.Fprintln(output, "⚠ Fibre metrics are off; run: tenderduty config set fibre_metrics.listen=<ip>:4318 fibre_metrics.token=generate (restart needed)")
	} else {
		fmt.Fprintf(output, "✓ Fibre metrics listen on %s\n", m.Listen)
	}
	host, _, _ := net.SplitHostPort(m.Listen)
	if m.Token == "" && !m.AllowWithoutToken && !fibreMetricsLoopback(host) {
		fmt.Fprintln(output, "⚠ Fibre metrics need a token; run: tenderduty config set fibre_metrics.token=generate")
	} else if m.Token != "" {
		fmt.Fprintln(output, "✓ Fibre metrics token set")
	} else if m.AllowWithoutToken {
		fmt.Fprintln(output, "⚠ Fibre metrics accept no token (allow_without_token); keep port 4318 firewalled to Fibre's IP")
	} else {
		fmt.Fprintln(output, "✓ Fibre metrics on loopback, no token needed")
	}
	settings := []string{name, "alerts.fibre_enabled=yes"}
	if hasInstance {
		settings = append(settings, "alerts.fibre_instance="+instance)
	}
	changed, err := setChains(options, settings, false, output)
	if err != nil {
		return err
	}
	if changed {
		reloadAfterEdit(options.StateFile, output, options.reload)
	}
	fmt.Fprintln(output, "✓ Fibre alerts enabled")
	printMetricsSetup(m, endpoint, chainID, false, output)
	return nil
}
