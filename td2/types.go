package tenderduty

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

const (
	showBLocks = 512
	staleHours = 24
)

// Config holds both the settings for tenderduty to monitor and state information while running.
type Config struct {
	alertChan      chan *alertMsg // channel used for outgoing notifications
	updateChan     chan *dash.ChainStatus
	logChan        chan dash.LogMessage
	statsChan      chan *promUpdate
	ctx            context.Context
	cancel         context.CancelFunc
	workers        sync.WaitGroup
	stateFile      string
	saveMux        sync.Mutex
	stateInPlace   bool   // state file can't be replaced, write it in place. guarded by saveMux
	savedSequence  uint64 // last queued notification known to be on disk, guarded by saveMux
	deliveryCtx    context.Context
	deliveryCancel context.CancelFunc

	// EnableDash enables the web dashboard
	EnableDash bool `yaml:"enable_dashboard"`
	// Listen is the URL for the dashboard to listen on, must be a valid/parsable URL
	Listen string `yaml:"listen_port"`
	// HideLogs controls whether logs are sent to the dashboard. It will also suppress many alarm details.
	// This is useful if the dashboard will be public.
	HideLogs bool `yaml:"hide_logs"`

	// NodeDownMin controls how long we wait before sending an alert that a node is not responding or has
	// fallen behind.
	NodeDownMin int `yaml:"node_down_alert_minutes"`
	// NodeDownSeverity controls the Pagerduty severity when notifying if a node is down.
	NodeDownSeverity string `yaml:"node_down_alert_severity"`

	// Prom controls if the prometheus exporter is enabled.
	Prom bool `yaml:"prometheus_enabled"`
	// PrometheusListenPort is the port number used by the prometheus web server
	PrometheusListenPort int `yaml:"prometheus_listen_port"`

	// Pagerduty configuration values
	Pagerduty PDConfig `yaml:"pagerduty"`
	// Discord webhook information
	Discord DiscordConfig `yaml:"discord"`
	// Telegram api information
	Telegram TeleConfig `yaml:"telegram"`
	// Slack webhook information
	Slack SlackConfig `yaml:"slack"`
	// Healthcheck information
	Healthcheck HealthcheckConfig `yaml:"healthcheck"`
	// AlertDefaults supplies shared settings before chain overrides are applied.
	AlertDefaults AlertConfig `yaml:"alert_defaults"`

	chainsMux sync.RWMutex // prevents concurrent map access for Chains
	// Chains has settings for each validator to monitor. The map's name does not need to match the chain-id.
	Chains map[string]*ChainConfig `yaml:"chains"`
}

// savedState is dumped to a JSON file at exit time, and is loaded at start. If successful it will prevent
// duplicate alerts, and will show old blocks in the dashboard.
type savedState struct {
	Heights     map[string]int64                `json:"heights,omitempty"`
	Alarms      *alarmCache                     `json:"alarms"`
	Blocks      map[string][]int                `json:"blocks"`
	LastBlocks  map[string]time.Time            `json:"last_blocks"`
	NodesDown   map[string]map[string]time.Time `json:"nodes_down"`
	Consecutive map[string]float64              `json:"consecutive_missed,omitempty"`
}

// ChainConfig represents a validator to be monitored on a chain, it is somewhat of a misnomer since multiple
// validators can be monitored on a single chain.
type ChainConfig struct {
	stateMux        sync.RWMutex
	validatorMux    sync.RWMutex
	refreshMux      sync.Mutex
	name            string
	client          *rpchttp.HTTP // legit tendermint client
	noNodes         bool          // tracks if all nodes are down
	rpcSkip         string
	monitoring      bool
	monitoringSince time.Time
	valInfo         *ValInfo // recent validator state, only refreshed every few minutes
	lastValInfo     *ValInfo // use for detecting newly-jailed/tombstone
	blocksResults   []int
	lastBlockTime   time.Time
	lastBlockAlarm  bool
	lastBlockNum    int64
	observedBlock   bool

	statTotalSigns      float64
	statTotalProps      float64
	statTotalMiss       float64
	statPrevoteMiss     float64
	statPrecommitMiss   float64
	statConsecutiveMiss float64

	// ChainId is used to ensure any endpoints contacted claim to be on the correct chain. This is a weak verification,
	// no light client validation is performed, so caution is advised when using public endpoints.
	ChainId string `yaml:"chain_id"`
	// ValAddress is the validator operator address to be monitored. Tenderduty v1 required the consensus address,
	// this is no longer needed. The operator address is much easier to find in explorers etc.
	ValAddress string `yaml:"valoper_address"`
	// Validators allows several validators to use the same chain and RPC settings.
	Validators []ValidatorConfig `yaml:"validators"`
	// ValconsOverride allows skipping the lookup of the consensus public key and setting it directly.
	ValconsOverride string `yaml:"valcons_override"`
	// ExtraInfo will be appended to the alert data. This is useful for pagerduty because multiple tenderduty instances
	// can be pointed at pagerduty and duplicate alerts will be filtered by using a key. The first alert will win, this
	// can be useful for knowing what tenderduty instance sent the alert.
	ExtraInfo string `yaml:"extra_info"` // FIXME not used yet!
	// Alerts defines the types of alerts to send for this chain.
	Alerts AlertConfig `yaml:"alerts"`
	// PublicFallback determines if tenderduty should attempt to use public RPC endpoints in the situation that not
	// explicitly defined RPC servers are available. Not recommended.
	PublicFallback bool `yaml:"public_fallback"`
	// Nodes defines what RPC servers to connect to.
	Nodes []*NodeConfig `yaml:"nodes"`
}

type ValidatorConfig struct {
	Name            string `yaml:"name"`
	ValAddress      string `yaml:"valoper_address"`
	ValconsOverride string `yaml:"valcons_override"`
	// Nodes are this validator's own RPC nodes.
	Nodes []*NodeConfig `yaml:"nodes"`
}

// mkUpdate returns the info needed by prometheus for a gauge.
func (cc *ChainConfig) mkUpdate(t metricType, v float64, node string) *promUpdate {
	info, _ := cc.validatorState()
	return &promUpdate{
		metric:   t,
		counter:  v,
		name:     cc.name,
		chainId:  cc.ChainId,
		moniker:  info.Moniker,
		endpoint: node,
	}
}

// AlertConfig defines the type of alerts to send for a ChainConfig
type AlertConfig struct {
	// How many minutes to wait before alerting that no new blocks have been seen
	Stalled int `yaml:"stalled_minutes"`
	// Whether to alert when no new blocks are seen
	StalledAlerts bool `yaml:"stalled_enabled"`

	// How many missed blocks are acceptable before alerting
	ConsecutiveMissed int `yaml:"consecutive_missed"`
	// Tag for pagerduty to set the alert priority
	ConsecutivePriority string `yaml:"consecutive_priority"`
	// Whether to alert on consecutive missed blocks
	ConsecutiveAlerts bool `yaml:"consecutive_enabled"`

	// Window is how many blocks missed as a percentage of the slashing window to trigger an alert
	Window int `yaml:"percentage_missed"`
	// PercentagePriority is a tag for pagerduty to route on priority
	PercentagePriority string `yaml:"percentage_priority"`
	// PercentageAlerts is whether to alert on percentage based misses
	PercentageAlerts bool `yaml:"percentage_enabled"`

	// AlertIfInactive decides if tenderduty send an alert if the validator is not in the active set?
	AlertIfInactive bool `yaml:"alert_if_inactive"`
	// AlertIfNoServers: should an alert be sent if no servers are reachable?
	AlertIfNoServers bool `yaml:"alert_if_no_servers"`

	// PagerdutyAlerts: Should pagerduty alerts be sent for this chain? Both 'config.pagerduty.enabled: yes' and this must be set.
	//Deprecated: use Pagerduty.Enabled instead
	PagerdutyAlerts bool `yaml:"pagerduty_alerts"`
	// DiscordAlerts: Should discord alerts be sent for this chain? Both 'config.discord.enabled: yes' and this must be set.
	//Deprecated: use Discord.Enabled instead
	DiscordAlerts bool `yaml:"discord_alerts"`
	// TelegramAlerts: Should telegram alerts be sent for this chain? Both 'config.telegram.enabled: yes' and this must be set.
	//Deprecated: use Telegram.Enabled instead
	TelegramAlerts bool `yaml:"telegram_alerts"`

	// chain specific overrides for alert destinations.
	// Pagerduty configuration values
	Pagerduty PDConfig `yaml:"pagerduty"`
	// Discord webhook information
	Discord DiscordConfig `yaml:"discord"`
	// Telegram webhook information
	Telegram TeleConfig `yaml:"telegram"`
	// Slack webhook information
	Slack SlackConfig `yaml:"slack"`
}

// NodeConfig holds the basic information for a node to connect to.
type NodeConfig struct {
	stateMux    sync.RWMutex
	Url         string `yaml:"url"`
	AlertIfDown bool   `yaml:"alert_if_down"`

	down      bool
	wasDown   bool
	syncing   bool
	checked   bool
	lastMsg   string
	downSince time.Time
}

// PDConfig is the information required to send alerts to PagerDuty
type PDConfig struct {
	Enabled         bool   `yaml:"enabled"`
	ApiKey          string `yaml:"api_key"`
	DefaultSeverity string `yaml:"default_severity"`
}

// DiscordConfig holds the information needed to publish to a Discord webhook for sending alerts
type DiscordConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Webhook  string   `yaml:"webhook"`
	Mentions []string `yaml:"mentions"`
}

// TeleConfig holds the information needed to publish to a Telegram webhook for sending alerts
type TeleConfig struct {
	Enabled  bool     `yaml:"enabled"`
	ApiKey   string   `yaml:"api_key"`
	Channel  string   `yaml:"channel"`
	Mentions []string `yaml:"mentions"`
}

// SlackConfig holds the information needed to publish to a Slack webhook for sending alerts
type SlackConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Webhook  string   `yaml:"webhook"`
	Mentions []string `yaml:"mentions"`
}

// HealthcheckConfig holds the information needed to send pings to a healthcheck endpoint
type HealthcheckConfig struct {
	Enabled  bool   `yaml:"enabled"`
	PingURL  string `yaml:"ping_url"`
	PingRate int64  `yaml:"ping_rate"`
}

// validateConfig is a non-exhaustive check for common problems with the configuration. Needs love.
func validateConfig(c *Config) (fatal bool, problems []string) {
	problems = make([]string, 0)

	if c.EnableDash {
		port, parseErr := strconv.Atoi(c.Listen)
		if parseErr != nil || port < 1 || port > 65535 {
			fatal = true
			problems = append(problems, fmt.Sprintf("error: The dashboard port %s is invalid", c.Listen))
		}
	}
	if c.Prom && (c.PrometheusListenPort < 1 || c.PrometheusListenPort > 65535) {
		fatal = true
		problems = append(problems, "error: The prometheus listen port is invalid")
	}
	if c.Healthcheck.Enabled && (c.Healthcheck.PingRate < 1 || c.Healthcheck.PingURL == "") {
		fatal = true
		problems = append(problems, "error: Healthcheck requires a ping URL and a positive rate")
	}
	if c.Healthcheck.Enabled && c.Healthcheck.PingRate > int64((1<<63-1)/time.Second) {
		fatal = true
		problems = append(problems, "error: Healthcheck ping rate is too large")
	}

	if c.Pagerduty.Enabled {
		rex := regexp.MustCompile(`[+_-]`)
		if rex.MatchString(c.Pagerduty.ApiKey) {
			fatal = true
			problems = append(problems, "error: The Pagerduty key provided appears to be an Oauth token, not a V2 Events API key.")
		}
	}

	if c.NodeDownMin < 1 {
		// older configs could leave this out
		c.NodeDownMin = 3
		problems = append(problems, "warning: 'node_down_alert_minutes' is not set, using 3 minutes")
	} else if c.NodeDownMin < 3 {
		problems = append(problems, "warning: setting 'node_down_alert_minutes' to less than three minutes might result in false alarms")
	}
	if c.NodeDownSeverity == "" {
		c.NodeDownSeverity = "critical"
	}

	for k, v := range c.Chains {
		if v == nil {
			fatal = true
			problems = append(problems, fmt.Sprintf("error: %s has no chain configuration", k))
			continue
		}
		if v.ChainId == "" || v.ValAddress == "" {
			fatal = true
			problems = append(problems, fmt.Sprintf("error: %s requires chain_id and valoper_address", k))
		}
		if len(v.Nodes) == 0 && !v.PublicFallback {
			fatal = true
			problems = append(problems, fmt.Sprintf("error: %s requires an RPC node or public_fallback", k))
		}
		for _, node := range v.Nodes {
			if node == nil {
				fatal = true
				problems = append(problems, fmt.Sprintf("error: %s has an empty RPC node", k))
				continue
			}
			parsed, parseErr := url.Parse(node.Url)
			if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "tcp") {
				// older versions accepted this
				problems = append(problems, fmt.Sprintf("warning: %s has an invalid RPC URL, it will be treated as down: %s", k, node.Url))
			}
		}
		// a zero threshold would alert all the time, use the example config's values
		if v.Alerts.StalledAlerts && v.Alerts.Stalled < 1 {
			v.Alerts.Stalled = 10
			problems = append(problems, fmt.Sprintf("warning: %s has no positive stalled_minutes, using 10", k))
		}
		if v.Alerts.ConsecutiveAlerts && v.Alerts.ConsecutiveMissed < 1 {
			v.Alerts.ConsecutiveMissed = 5
			problems = append(problems, fmt.Sprintf("warning: %s has no positive consecutive_missed, using 5", k))
		}
		if v.Alerts.PercentageAlerts && (v.Alerts.Window < 1 || v.Alerts.Window > 100) {
			v.Alerts.Window = 10
			problems = append(problems, fmt.Sprintf("warning: %s percentage_missed is not between 1 and 100, using 10", k))
		}
		if len(v.blocksResults) != showBLocks {
			blocks := make([]int, showBLocks)
			for i := range blocks {
				blocks[i] = -1
			}
			copy(blocks, v.blocksResults)
			v.blocksResults = blocks
		}
		if v.name == "" {
			v.name = k
		}

		v.valInfo = &ValInfo{Moniker: "not connected"}

		// the bools for enabling alerts are deprecated with full configs preferred,
		// don't break if someone is still using them:
		if v.Alerts.DiscordAlerts && !v.Alerts.Discord.Enabled {
			v.Alerts.Discord.Enabled = true
		}
		if v.Alerts.TelegramAlerts && !v.Alerts.Telegram.Enabled {
			v.Alerts.Telegram.Enabled = true
		}
		if v.Alerts.PagerdutyAlerts && !v.Alerts.Pagerduty.Enabled {
			v.Alerts.Pagerduty.Enabled = true
		}

		// if the settings are blank, copy in the defaults:
		if v.Alerts.Discord.Webhook == "" {
			v.Alerts.Discord.Webhook = c.Discord.Webhook
			v.Alerts.Discord.Mentions = c.Discord.Mentions
		}
		if v.Alerts.Slack.Webhook == "" {
			v.Alerts.Slack.Webhook = c.Slack.Webhook
			v.Alerts.Slack.Mentions = c.Slack.Mentions
		}
		if v.Alerts.Telegram.ApiKey == "" {
			v.Alerts.Telegram.ApiKey = c.Telegram.ApiKey
			v.Alerts.Telegram.Mentions = c.Telegram.Mentions
		}
		if v.Alerts.Telegram.Channel == "" {
			v.Alerts.Telegram.Channel = c.Telegram.Channel
		}
		if v.Alerts.Pagerduty.ApiKey == "" {
			v.Alerts.Pagerduty.ApiKey = c.Pagerduty.ApiKey
			v.Alerts.Pagerduty.DefaultSeverity = c.Pagerduty.DefaultSeverity
		}
		if c.Telegram.Enabled && v.Alerts.Telegram.Enabled && (v.Alerts.Telegram.ApiKey == "" || v.Alerts.Telegram.Channel == "") {
			problems = append(problems, fmt.Sprintf("warning: %s telegram alerts need an API key and channel, they will not be sent", k))
		}
		if c.Discord.Enabled && v.Alerts.Discord.Enabled && !validHTTPURL(v.Alerts.Discord.Webhook) {
			problems = append(problems, fmt.Sprintf("warning: %s discord alerts need a valid webhook URL, they will not be sent", k))
		}
		if c.Slack.Enabled && v.Alerts.Slack.Enabled && !validHTTPURL(v.Alerts.Slack.Webhook) {
			problems = append(problems, fmt.Sprintf("warning: %s slack alerts need a valid webhook URL, they will not be sent", k))
		}
		if c.Pagerduty.Enabled && v.Alerts.Pagerduty.Enabled && v.Alerts.Pagerduty.ApiKey == "" {
			problems = append(problems, fmt.Sprintf("warning: %s pagerduty alerts need an API key, they will not be sent", k))
		}
		if v.Alerts.ConsecutivePriority == "" {
			v.Alerts.ConsecutivePriority = "critical"
		}
		if v.Alerts.PercentagePriority == "" {
			v.Alerts.PercentagePriority = "warning"
		}

		if v.Alerts.Slack.Enabled && !c.Slack.Enabled {
			problems = append(problems, fmt.Sprintf("warn: %20s is configured for slack alerts, but it is not enabled", k))
		}
		if v.Alerts.Discord.Enabled && !c.Discord.Enabled {
			problems = append(problems, fmt.Sprintf("warn: %20s is configured for discord alerts, but it is not enabled", k))
		}
		if v.Alerts.Pagerduty.Enabled && !c.Pagerduty.Enabled {
			problems = append(problems, fmt.Sprintf("warn: %20s is configured for pagerduty alerts, but it is not enabled", k))
		}
		if v.Alerts.Telegram.Enabled && !c.Telegram.Enabled {
			problems = append(problems, fmt.Sprintf("warn: %20s is configured for telegram alerts, but it is not enabled", k))
		}
		if !v.Alerts.ConsecutiveAlerts && !v.Alerts.PercentageAlerts && !v.Alerts.AlertIfInactive && !v.Alerts.AlertIfNoServers && !v.Alerts.StalledAlerts {
			problems = append(problems, fmt.Sprintf("warn: %20s has no alert types configured", k))
		}
		if !v.Alerts.Pagerduty.Enabled && !v.Alerts.Discord.Enabled && !v.Alerts.Telegram.Enabled && !v.Alerts.Slack.Enabled {
			problems = append(problems, fmt.Sprintf("warn: %20s has no notifications configured", k))
		}
		if c.EnableDash {
			c.updateChan <- &dash.ChainStatus{
				MsgType:      "status",
				Name:         v.name,
				ChainId:      v.ChainId,
				Moniker:      v.valInfo.Moniker,
				Bonded:       v.valInfo.Bonded,
				Jailed:       v.valInfo.Jailed,
				Tombstoned:   v.valInfo.Tombstoned,
				Missed:       v.valInfo.Missed,
				Window:       v.valInfo.Window,
				Nodes:        len(v.Nodes),
				HealthyNodes: 0,
				ActiveAlerts: 0,
				Blocks:       v.blocksResults,
			}
		}
	}

	return
}

func loadChainConfig(yamlFile string) (*ChainConfig, error) {
	//#nosec -- variable specified on command line
	b, e := os.ReadFile(yamlFile)
	if e != nil {
		return nil, e
	}
	c := &ChainConfig{}
	e = unmarshalConfig(yamlFile, b, c, &ChainConfig{})
	if e != nil {
		return nil, e
	}
	return c, nil
}

// unmarshalConfig ignores unknown settings like older versions did, but warns about them so typos get noticed.
func unmarshalConfig(name string, data []byte, out, check interface{}) error {
	if err := unmarshalLenient(data, out); err != nil {
		return err
	}
	if err := decodeConfig(data, check); err != nil {
		l("⚠️", name, "has settings that are not understood and will be ignored:", err)
	}
	return nil
}

// loadConfig creates a new Config from a file.
// addDefaultPorts adds :443 or :80 to RPC URLs without a port, the RPC client needs one
func addDefaultPorts(c *Config) {
	for _, chain := range c.Chains {
		for _, node := range chain.Nodes {
			if node != nil {
				node.Url = withDefaultPort(node.Url)
			}
		}
	}
}

func withDefaultPort(rpcUrl string) string {
	u, err := url.Parse(rpcUrl)
	if err != nil || u.Host == "" || u.Port() != "" {
		return rpcUrl
	}
	port := map[string]string{"https": "443", "http": "80"}[u.Scheme]
	if port == "" {
		return rpcUrl
	}
	return strings.Replace(rpcUrl, u.Host, net.JoinHostPort(u.Hostname(), port), 1)
}

func loadConfig(yamlFile, stateFile, chainConfigDirectory string, password *string) (*Config, error) {

	c := &Config{}
	var configData []byte
	if strings.HasPrefix(yamlFile, "http://") || strings.HasPrefix(yamlFile, "https://") {
		if *password == "" {
			return nil, errors.New("a password is required if loading a remote configuration")
		}
		//#nosec -- url is specified on command line
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(yamlFile)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("could not load remote configuration: status %d", resp.StatusCode)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		log.Printf("downloaded %d bytes from %s", len(b), yamlFile)
		decrypted, err := decrypt(b, *password)
		if err != nil {
			return nil, err
		}
		empty := ""
		password = &empty             // let gc get password out of memory, it's still referenced in main()
		_ = os.Setenv("PASSWORD", "") // also clear the ENV var
		err = unmarshalConfig("remote config", decrypted, c, &Config{})
		if err != nil {
			return nil, err
		}
		configData = decrypted
	} else {
		//#nosec -- variable specified on command line
		b, e := os.ReadFile(yamlFile)
		if e != nil {
			return nil, e
		}
		e = unmarshalConfig(yamlFile, b, c, &Config{})
		if e != nil {
			return nil, e
		}
		configData = b
	}

	// Load additional chain configuration files
	chainFiles := make(map[string][]byte)
	chainConfigFiles, e := os.ReadDir(chainConfigDirectory)
	if e != nil {
		l("Failed to scan chainConfigDirectory", e)
	}

	for _, chainConfigFile := range chainConfigFiles {
		if chainConfigFile.IsDir() {
			l("Skipping Directory: ", chainConfigFile.Name())
			continue
		}
		if !strings.HasSuffix(chainConfigFile.Name(), ".yml") {
			l("Skipping non .yml file: ", chainConfigFile.Name())
			continue
		}
		fmt.Println("Reading Chain Config File: ", chainConfigFile.Name())
		chainConfig, e := loadChainConfig(path.Join(chainConfigDirectory, chainConfigFile.Name()))
		if e != nil {
			l(fmt.Sprintf("Failed to read %s", chainConfigFile), e)
			return nil, e
		}

		// same naming as older versions so saved state matches: osmosis.mainnet.yml is "osmosis"
		chainName := strings.Split(chainConfigFile.Name(), ".")[0]
		if c.Chains[chainName] != nil {
			l("⚠️", chainConfigFile.Name(), "replaces the existing configuration for", chainName)
		}
		//#nosec -- variable specified on command line
		chainFiles[chainName], e = os.ReadFile(path.Join(chainConfigDirectory, chainConfigFile.Name()))
		if e != nil {
			return nil, e
		}

		// Create map if it didnt exist in config.yml
		if c.Chains == nil {
			c.Chains = make(map[string]*ChainConfig)
		}
		c.Chains[chainName] = chainConfig
		l(fmt.Sprintf("Added %s from ", chainName), chainConfigFile.Name())
	}

	if len(c.Chains) == 0 {
		return nil, errors.New("no chains configured")
	}
	if err := applyAlertDefaults(c, configData, chainFiles); err != nil {
		return nil, err
	}
	if err := expandValidators(c); err != nil {
		return nil, err
	}
	addDefaultPorts(c)

	c.alertChan = make(chan *alertMsg, len(c.Chains)*4)
	c.logChan = make(chan dash.LogMessage, 128)
	// buffer enough to get through validateConfig()
	c.updateChan = make(chan *dash.ChainStatus, len(c.Chains)*2)
	c.statsChan = make(chan *promUpdate, len(c.Chains)*2)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.deliveryCtx, c.deliveryCancel = context.WithCancel(context.Background())
	c.stateFile = stateFile

	alarms = newAlarmCache()
	saved := &savedState{}
	//#nosec -- variable specified on command line
	b, e := os.ReadFile(stateFile)
	switch {
	case e == nil && len(bytes.TrimSpace(b)) > 0:
		if e = json.Unmarshal(b, saved); e != nil {
			// a broken state file shouldn't stop monitoring, just start fresh
			l("⚠️ could not decode saved state, starting without it", e)
			saved = &savedState{}
		}
	case e != nil && !os.IsNotExist(e):
		l("⚠️ could not load saved state, starting without it", e)
	}
	for k, v := range saved.Blocks {
		if c.Chains[k] != nil {
			c.Chains[k].blocksResults = v
			c.Chains[k].observedBlock = len(v) > 0 && v[0] >= 0
			for _, status := range v {
				if status < 0 || status >= 3 {
					break
				}
				c.Chains[k].statConsecutiveMiss++
			}
		}
	}
	// the block history can't tell a gap apart from a reset, use the saved counter if we have it
	for k, missed := range saved.Consecutive {
		if c.Chains[k] != nil {
			c.Chains[k].statConsecutiveMiss = missed
		}
	}
	for k, observed := range saved.LastBlocks {
		if c.Chains[k] != nil {
			c.Chains[k].lastBlockTime = observed
		}
	}

	for name, height := range saved.Heights {
		if c.Chains[name] != nil {
			c.Chains[name].lastBlockNum = height
		}
	}
	// restore alarm state to prevent duplicate alerts
	if saved.Alarms != nil {
		alarms.Outbox = saved.Alarms.Outbox
		alarms.NextNotification = saved.Alarms.NextNotification
		if saved.Alarms.PendingRecoveries != nil {
			alarms.PendingRecoveries = saved.Alarms.PendingRecoveries
		}
		if saved.Alarms.SentTgAlarms != nil {
			alarms.SentTgAlarms = saved.Alarms.SentTgAlarms
			clearStale(alarms.SentTgAlarms, "telegram", c.Pagerduty.Enabled, staleHours)
		}
		if saved.Alarms.SentPdAlarms != nil {
			alarms.SentPdAlarms = saved.Alarms.SentPdAlarms
			clearStale(alarms.SentPdAlarms, "PagerDuty", c.Pagerduty.Enabled, staleHours)
		}
		if saved.Alarms.SentDiAlarms != nil {
			alarms.SentDiAlarms = saved.Alarms.SentDiAlarms
			clearStale(alarms.SentDiAlarms, "Discord", c.Pagerduty.Enabled, staleHours)
		}
		if saved.Alarms.SentSlkAlarms != nil {
			alarms.SentSlkAlarms = saved.Alarms.SentSlkAlarms
			clearStale(alarms.SentSlkAlarms, "Slack", c.Pagerduty.Enabled, staleHours)
		}
		if saved.Alarms.AllAlarms != nil {
			alarms.AllAlarms = saved.Alarms.AllAlarms
			for _, alrm := range saved.Alarms.AllAlarms {
				clearStale(alrm, "dashboard", c.Pagerduty.Enabled, staleHours)
			}
		}
	}

	// we need to know if the node was already down to clear alarms
	if saved.NodesDown != nil {
		for k, v := range saved.NodesDown {
			for nodeUrl := range v {
				if !v[nodeUrl].IsZero() {
					if c.Chains[k] != nil {
						for j := range c.Chains[k].Nodes {
							if c.Chains[k].Nodes[j].Url == nodeUrl {
								c.Chains[k].Nodes[j].down = true
								c.Chains[k].Nodes[j].wasDown = true
								c.Chains[k].Nodes[j].downSince = v[nodeUrl]
							}
						}
					}
				}
			}
		}
		// now we need to know if all RPC endpoints were down.
		for k, v := range c.Chains {
			downCount := 0
			for j := range v.Nodes {
				if v.Nodes[j].down {
					downCount += 1
				}
			}
			if downCount == len(c.Chains[k].Nodes) {
				c.Chains[k].noNodes = true
			}
		}
	}

	return c, nil
}

func clearStale(alarms map[string]time.Time, what string, hasPagerduty bool, hours float64) {
	for k := range alarms {
		if hasPendingRecovery(k) {
			continue
		}
		if time.Since(alarms[k]).Hours() >= hours {
			l(fmt.Sprintf("🗑 not restoring old alarm (%v >%.2f hours) from cache - %s", alarms[k], hours, k))
			if hasPagerduty && what == "pagerduty" {
				l("NOTE: stale alarms may need to be manually cleared from PagerDuty!")
			}
			delete(alarms, k)
			continue
		}
		l("📂 restored", what, "alarm state -", k)
	}
}
