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
	Signed         int64  `json:"signed"`
	Proposed       int64  `json:"proposed"`
	MissedProps    int64  `json:"missed_proposals"`
	VotingPower    int64  `json:"voting_power"`
	TotalPower     int64  `json:"total_power"`
	Rank           int    `json:"rank"`
	ActiveSet      int    `json:"active_set"`
	MaxValidators  int    `json:"max_validators"`
	MaxMissed      int64  `json:"max_missed"`

	Blocks []int `json:"blocks"`

	Fibre *FibreStatus `json:"fibre,omitempty"`
}

type FibreStatus struct {
	Resources     map[string]float64 `json:"resources,omitempty"`
	Registered    bool               `json:"registered"`
	Host          string             `json:"host,omitempty"`
	Reachable     bool               `json:"reachable"`
	DownSince     int64              `json:"down_since,omitempty"` // unix seconds
	DownReason    string             `json:"down_reason,omitempty"`
	Identity      string             `json:"identity"` // verified, failed or unchecked
	IdentityError string             `json:"identity_error,omitempty"`
	Version       string             `json:"version,omitempty"`

	// ChainBudget 0 is none: not in the active set, or the parameter not read yet.
	InActiveSet    bool    `json:"in_active_set"`
	Rows           int     `json:"rows"`
	TotalRows      int     `json:"total_rows"`
	RowPercent     float64 `json:"row_percent"`
	MinimumApplies bool    `json:"minimum_applies"`
	ChainBudget    int64   `json:"chain_budget,omitempty"`

	// Reported by Fibre itself. A budget of 0 is --unlimited-budget.
	MetricsReceived   bool  `json:"metrics_received"`
	MetricsAt         int64 `json:"metrics_at,omitempty"`
	BudgetReported    bool  `json:"budget_reported"`
	FibreBudget       int64 `json:"fibre_budget"`
	Unlimited         bool  `json:"unlimited"`
	OccupancyKnown    bool  `json:"occupancy_known"`
	Occupancy         int64 `json:"occupancy"`
	UploadsInFlight   int64 `json:"uploads_in_flight"`
	DownloadsInFlight int64 `json:"downloads_in_flight"`
	LastUploadAt      int64 `json:"last_upload_at,omitempty"`

	Last10m FibreWindow `json:"last_10m"`
	Last24h FibreWindow `json:"last_24h"`
}

// Seconds is the part of the window seen, short after a restart. A nil average or share was not measured.
type FibreWindow struct {
	Seconds          int64    `json:"seconds"`
	UploadsOK        int64    `json:"uploads_ok"`
	UploadsFailed    int64    `json:"uploads_failed"`
	UploadsRejected  int64    `json:"uploads_rejected"`
	BytesReceived    int64    `json:"bytes_received"`
	AvgUploadMs      *float64 `json:"avg_upload_ms,omitempty"`
	DownloadsOK      int64    `json:"downloads_ok"`
	DownloadsFailed  int64    `json:"downloads_failed"`
	BytesServed      int64    `json:"bytes_served"`
	AvgDownloadMs    *float64 `json:"avg_download_ms,omitempty"`
	Signatures       int64    `json:"signatures"`
	SignaturesFailed int64    `json:"signatures_failed"`
	SignFastPercent  *float64 `json:"sign_fast_percent,omitempty"` // of the successful ones, at or under 10 ms
	AvgSignMs        *float64 `json:"avg_sign_ms,omitempty"`
	StorePutErrors   int64    `json:"store_put_errors"`
	StoreGetErrors   int64    `json:"store_get_errors"`
	AvgPutMs         *float64 `json:"avg_put_ms,omitempty"`
	AvgGetMs         *float64 `json:"avg_get_ms,omitempty"`
	PruneErrors      int64    `json:"prune_errors"`
	EntriesPruned    int64    `json:"entries_pruned"`
	AvgPruneMs       *float64 `json:"avg_prune_ms,omitempty"`
	ObjectErrors     int64    `json:"object_storage_errors"` // timed out, throttled or failed
	ObjectRequests   int64    `json:"object_storage_requests"`
}

type LogMessage struct {
	MsgType string `json:"msgType"`
	Ts      int64  `json:"ts"`
	Msg     string `json:"msg"`
}
