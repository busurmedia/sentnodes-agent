package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	DefaultServerURL = "https://api.sentnodes.com"
	// Same path the node uses, so the agent's config path equals the node's.
	DefaultNodeHome = "/root/.sentinel-dvpnx"
	// Fallback CometBFT RPC when the node config has none reachable.
	FallbackRPC       = "https://rpc-sentinel.busurnode.com"
	PollInterval      = time.Minute
	HeartbeatInterval = time.Minute
	MetricsInterval   = 10 * time.Minute

	UdvpnPerP2P uint64 = 1_000_000

	// Withdrawal safety defaults (udvpn; env values are given in P2P). Both are
	// reported to SentNodes so the SentNodes guard matches the operator's setting.
	DefaultWithdrawMinUdvpn     uint64 = 250 * UdvpnPerP2P // 250 P2P, smallest single withdrawal
	DefaultWithdrawReserveUdvpn uint64 = 50 * UdvpnPerP2P  // 50 P2P, balance always kept

	// Hard floor for both limits (anti-spam / keep operating funds); lower values are raised to it.
	WithdrawFloorUdvpn uint64 = 50 * UdvpnPerP2P // 50 P2P
)

// Config is the agent's resolved configuration: a few operator env vars plus the
// fields it reads from the node's own config.toml.
type Config struct {
	ServerURL       string
	APIKey          string
	WithdrawAddr    string
	WithdrawMin     uint64
	WithdrawReserve uint64
	NodeContainer   string
	NodeHome        string

	KeyringBackend string
	KeyringName    string
	FromName       string
	ChainID        string
	RPCAddrs       []string
	Moniker        string
	GigabytePrices string
	HourlyPrices   string
	Oracle         string
	Gas            uint64
	GasPrices      string
}

type nodeToml struct {
	Keyring struct {
		Backend string `toml:"backend"`
		Name    string `toml:"name"`
	} `toml:"keyring"`
	RPC struct {
		Addrs   []string `toml:"addrs"`
		ChainID string   `toml:"chain_id"`
	} `toml:"rpc"`
	Tx struct {
		FromName  string `toml:"from_name"`
		Gas       uint64 `toml:"gas"`
		GasPrices string `toml:"gas_prices"`
	} `toml:"tx"`
	Node struct {
		Moniker        string `toml:"moniker"`
		GigabytePrices string `toml:"gigabyte_prices"`
		HourlyPrices   string `toml:"hourly_prices"`
	} `toml:"node"`
	Oracle struct {
		Name string `toml:"name"`
	} `toml:"oracle"`
}

func Load() (*Config, error) {
	c := &Config{
		ServerURL:     env("SENTNODES_SERVER_URL", DefaultServerURL),
		APIKey:        os.Getenv("SENTNODES_AGENT_KEY"),
		WithdrawAddr:  os.Getenv("WITHDRAWAL_ADDRESS"),
		NodeContainer: os.Getenv("DVPN_NODE_CONTAINER"),
		NodeHome:      env("DVPN_NODE_HOME", DefaultNodeHome),
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("SENTNODES_AGENT_KEY is required")
	}
	c.WithdrawMin = envP2PUdvpn("WITHDRAWAL_MIN", DefaultWithdrawMinUdvpn)
	c.WithdrawReserve = envP2PUdvpn("WITHDRAWAL_RESERVE", DefaultWithdrawReserveUdvpn)

	path := filepath.Join(c.NodeHome, "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("node config not found - make sure you've mounted the correct Sentinel dVPN node volume")
		}
		return nil, fmt.Errorf("cannot read node config at %s: %w", path, err)
	}
	var nt nodeToml
	if err := toml.Unmarshal(data, &nt); err != nil {
		return nil, fmt.Errorf("parsing config.toml: %w", err)
	}

	c.KeyringBackend = nt.Keyring.Backend
	c.KeyringName = nt.Keyring.Name
	c.FromName = nt.Tx.FromName
	c.ChainID = nt.RPC.ChainID
	c.RPCAddrs = nt.RPC.Addrs
	c.Moniker = nt.Node.Moniker
	c.GigabytePrices = nt.Node.GigabytePrices
	c.HourlyPrices = nt.Node.HourlyPrices
	c.Oracle = nt.Oracle.Name
	c.Gas = nt.Tx.Gas
	if c.Gas == 0 {
		c.Gas = 200000
	}
	c.GasPrices = nt.Tx.GasPrices
	return c, nil
}

// ConfigPath is the node config.toml path.
func (c *Config) ConfigPath() string { return filepath.Join(c.NodeHome, "config.toml") }

// RPCEndpoints is the node's configured CometBFT RPC addresses plus the fallback,
// tried in order.
func (c *Config) RPCEndpoints() []string {
	out := append([]string{}, c.RPCAddrs...)
	return append(out, FallbackRPC)
}

// StatePath is where the agent persists its per-agent token.
func (c *Config) StatePath() string { return filepath.Join(c.NodeHome, ".sentagent", "token") }

// NodeConfigSnapshot re-reads config.toml for the current price/RPC/oracle values
// (the startup copy goes stale after an edit). Heartbeat sends these to prefill the forms.
func (c *Config) NodeConfigSnapshot() (moniker, gigabyte, hourly, oracle string, rpc []string) {
	data, err := os.ReadFile(c.ConfigPath())
	if err != nil {
		return "", "", "", "", nil
	}
	var nt nodeToml
	if err := toml.Unmarshal(data, &nt); err != nil {
		return "", "", "", "", nil
	}
	return nt.Node.Moniker, nt.Node.GigabytePrices, nt.Node.HourlyPrices, nt.Oracle.Name, nt.RPC.Addrs
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// envP2PUdvpn reads an env value expressed in P2P and returns it in udvpn.
func envP2PUdvpn(k string, d uint64) uint64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return uint64(f * float64(UdvpnPerP2P))
		}
	}
	return d
}
