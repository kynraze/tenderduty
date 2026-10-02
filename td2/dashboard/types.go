package dash

type HealthStatus struct {
	Alive       bool `json:"alive"`
	Ready       bool `json:"ready"`
	Chains      int  `json:"chains"`
	Validators  int  `json:"validators"`
	Monitoring  int  `json:"monitoring"`
	Unavailable int  `json:"rpc_unavailable"`
	Stale       int  `json:"stale_validator_info"`
}

type ChainStatus struct {
	MsgType        string `json:"msgType"`
	Name           string `json:"name"`
	ChainId        string `json:"chain_id"`
	Moniker        string `json:"moniker"`
	Bonded         bool   `json:"bonded"`
	Jailed         bool   `json:"jailed"`
	Tombstoned     bool   `json:"tombstoned"`
	Missed         int64  `json:"missed"`
	Window         int64  `json:"window"`
	SigningStale   bool   `json:"signing_stale"`
	ValidatorStale bool   `json:"validator_stale"`
	Monitoring     bool   `json:"monitoring"`
	Nodes          int    `json:"nodes"`
	HealthyNodes   int    `json:"healthy_nodes"`
	NoNodes        bool   `json:"no_nodes"`
	ActiveAlerts   int    `json:"active_alerts"`
	Height         int64  `json:"height"`
	LastBlockAt    int64  `json:"last_block_at"`
	LastError      string `json:"last_error"`

	Blocks []int `json:"blocks"`
}

type LogMessage struct {
	MsgType string `json:"msgType"`
	Ts      int64  `json:"ts"`
	Msg     string `json:"msg"`
}
