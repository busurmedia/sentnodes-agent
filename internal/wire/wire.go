// Package wire holds the JSON shapes exchanged with the SentNodes API. It is the
// only contract the agent knows about the server; no server internals belong here.
package wire

import "encoding/json"

type Envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Errors  *struct {
		Code    interface{} `json:"code"`
		Message string      `json:"message"`
	} `json:"errors"`
}

type ChallengeData struct {
	Nonce string `json:"nonce"`
	TTL   int    `json:"ttl"`
}

type RegisterReq struct {
	NodeAddr            string `json:"nodeAddr"`
	OperatorAddr        string `json:"operatorAddr"`
	Pubkey              string `json:"pubkey"`
	Signature           string `json:"signature"`
	HostId              string `json:"hostId,omitempty"`
	NodeContainer       string `json:"nodeContainer,omitempty"`
	Version             string `json:"version,omitempty"`
	WithdrawDestination string `json:"withdrawDestination,omitempty"`
	WithdrawMin         uint64 `json:"withdrawMin"`
	WithdrawReserve     uint64 `json:"withdrawReserve"`
}

type RegisterData struct {
	AgentToken string `json:"agentToken"`
	AgentID    int64  `json:"agentId"`
}

type Command struct {
	ID      string                 `json:"id"` // ULID (server PK); used for the result call + withdraw idempotency
	Type    string                 `json:"type"`
	Payload map[string]interface{} `json:"payload"`
}

type PollData struct {
	Commands      []Command `json:"commands"`
	LatestVersion string    `json:"latestVersion"`
}

type ResultReq struct {
	OK     bool                   `json:"ok"`
	Result map[string]interface{} `json:"result,omitempty"`
}

type Metrics struct {
	HostId      string  `json:"hostId,omitempty"`
	CPUModel    string  `json:"cpuModel,omitempty"`
	CPUCores    int     `json:"cpuCores,omitempty"`
	CPUPct      float64 `json:"cpuPct"`
	LoadAvg1    float64 `json:"loadAvg1"`
	MemUsed     uint64  `json:"memUsed"`
	MemTotal    uint64  `json:"memTotal"`
	SwapUsed    uint64  `json:"swapUsed"`
	SwapTotal   uint64  `json:"swapTotal"`
	DiskUsed    uint64  `json:"diskUsed"`
	DiskTotal   uint64  `json:"diskTotal"`
	NetRxBytes  uint64  `json:"netRxBytes"`
	NetTxBytes  uint64  `json:"netTxBytes"`
	NodeCPUPct  float64 `json:"nodeCpuPct"`
	NodeMemUsed uint64  `json:"nodeMemUsed"`
	UptimeSec   uint64  `json:"uptimeSec"`
	OS          string  `json:"os,omitempty"`
	Kernel      string  `json:"kernel,omitempty"`
}

type MetricsReq struct {
	Metrics Metrics `json:"metrics"`
}

type Heartbeat struct {
	Version             string          `json:"version,omitempty"`
	NodeContainer       string          `json:"nodeContainer,omitempty"`
	WithdrawDestination string          `json:"withdrawDestination,omitempty"`
	WithdrawMin         uint64          `json:"withdrawMin"`
	WithdrawReserve     uint64          `json:"withdrawReserve"`
	Moniker             string          `json:"moniker,omitempty"`
	GigabytePrices      string          `json:"gigabytePrices,omitempty"`
	HourlyPrices        string          `json:"hourlyPrices,omitempty"`
	Oracle              string          `json:"oracle,omitempty"`
	RPCAddrs            []string        `json:"rpcAddrs,omitempty"`
	HostId              string          `json:"hostId,omitempty"`
	HealthChecks        map[string]bool `json:"healthChecks,omitempty"`
	NodeState           string          `json:"nodeState,omitempty"`
	NodeHealth          string          `json:"nodeHealth,omitempty"`
	NodeExitCode        *int            `json:"nodeExitCode,omitempty"`
	NodeRestartCount    *int            `json:"nodeRestartCount,omitempty"`
	NodeStartedAt       string          `json:"nodeStartedAt,omitempty"`
}
