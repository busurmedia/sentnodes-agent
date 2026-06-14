// Package exec runs command intents (status, prices, RPC, oracle, withdraw) locally.
// Server payloads are untrusted: inputs are validated and config.toml edits are surgical.
package exec

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/busurmedia/sentnodes-agent/internal/chain"
	"github.com/busurmedia/sentnodes-agent/internal/config"
	"github.com/busurmedia/sentnodes-agent/internal/keys"
)

// Status reports the node's identity and current local config (read-only).
func Status(cfg *config.Config, acct *keys.Account) map[string]interface{} {
	return map[string]interface{}{
		"operator":       acct.OperatorAddr(),
		"node":           acct.NodeAddr(),
		"gigabytePrices": cfg.GigabytePrices,
		"hourlyPrices":   cfg.HourlyPrices,
		"rpcAddrs":       cfg.RPCAddrs,
		"chainId":        cfg.ChainID,
	}
}

// EditPrices writes node.gigabyte_prices / node.hourly_prices; the caller restarts the node.
func EditPrices(cfg *config.Config, gigabyte, hourly string) error {
	return applyConfigEdit(cfg.ConfigPath(),
		[]tomlEdit{
			{table: "node", key: "gigabyte_prices", literal: tomlString(gigabyte)},
			{table: "node", key: "hourly_prices", literal: tomlString(hourly)},
		},
		func(m map[string]interface{}) error {
			node, ok := m["node"].(map[string]interface{})
			if !ok {
				return errors.New("config.toml has no [node] table")
			}
			node["gigabyte_prices"] = gigabyte
			node["hourly_prices"] = hourly
			return nil
		})
}

// EditRPC rewrites rpc.addrs; the caller restarts the node.
func EditRPC(cfg *config.Config, addrs []string) error {
	return applyConfigEdit(cfg.ConfigPath(),
		[]tomlEdit{{table: "rpc", key: "addrs", literal: tomlStringArray(addrs)}},
		func(m map[string]interface{}) error {
			rpc, ok := m["rpc"].(map[string]interface{})
			if !ok {
				return errors.New("config.toml has no [rpc] table")
			}
			rpc["addrs"] = addrs
			return nil
		})
}

// EditMoniker writes node.moniker (the node's display name); the caller restarts the node.
func EditMoniker(cfg *config.Config, moniker string) error {
	return applyConfigEdit(cfg.ConfigPath(),
		[]tomlEdit{{table: "node", key: "moniker", literal: tomlString(moniker)}},
		func(m map[string]interface{}) error {
			node, ok := m["node"].(map[string]interface{})
			if !ok {
				return errors.New("config.toml has no [node] table")
			}
			node["moniker"] = moniker
			return nil
		})
}

// DefaultOsmosisLCD is used when osmosis is selected but no valid endpoint is set.
const DefaultOsmosisLCD = "https://lcd.osmosis.zone:443"

var httpURLWithPort = regexp.MustCompile(`^https?://[^/\s:]+:\d{1,5}(/\S*)?$`)

// EditOracle sets oracle.name (coingecko, osmosis, or "" to disable). For osmosis it
// also defaults oracle.osmosis.api_addr when the current one is missing or invalid;
// coingecko's api_key is left untouched (not required).
func EditOracle(cfg *config.Config, name string) error {
	edits := []tomlEdit{{table: "oracle", key: "name", literal: tomlString(name)}}
	setOsmosisDefault := name == "osmosis" && !httpURLWithPort.MatchString(readOsmosisAddr(cfg.ConfigPath()))
	if setOsmosisDefault {
		edits = append(edits, tomlEdit{table: "oracle.osmosis", key: "api_addr", literal: tomlString(DefaultOsmosisLCD)})
	}
	return applyConfigEdit(cfg.ConfigPath(), edits, func(m map[string]interface{}) error {
		oracle, ok := m["oracle"].(map[string]interface{})
		if !ok {
			return errors.New("config.toml has no [oracle] table")
		}
		oracle["name"] = name
		if setOsmosisDefault {
			osm, _ := oracle["osmosis"].(map[string]interface{})
			if osm == nil {
				osm = map[string]interface{}{}
				oracle["osmosis"] = osm
			}
			osm["api_addr"] = DefaultOsmosisLCD
		}
		return nil
	})
}

// readOsmosisAddr returns the current oracle.osmosis.api_addr, or "" if unreadable.
func readOsmosisAddr(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var t struct {
		Oracle struct {
			Osmosis struct {
				APIAddr string `toml:"api_addr"`
			} `toml:"osmosis"`
		} `toml:"oracle"`
	}
	if err := toml.Unmarshal(data, &t); err != nil {
		return ""
	}
	return t.Oracle.Osmosis.APIAddr
}

// Withdraw sends udvpn from the operator account to the env-pinned destination (never the
// server). When all is true it withdraws everything above the reserve, computed from the
// live balance; idemKey guards double-sends via a local log and the on-chain sequence.
func Withdraw(ctx context.Context, cfg *config.Config, acct *keys.Account, amount uint64, all bool, idemKey string) (string, error) {
	if cfg.WithdrawAddr == "" {
		return "", errors.New("withdrawals disabled: WITHDRAWAL_ADDRESS is not set")
	}

	logPath := filepath.Join(cfg.NodeHome, ".sentagent", "withdrawals")
	if h := lookupWithdraw(logPath, idemKey); h != "" {
		return h, nil
	}

	c := chain.New(cfg.RPCEndpoints())
	fee := computeFee(cfg.Gas, cfg.GasPrices)
	bal, err := c.Balance(ctx, acct.OperatorAddr(), "udvpn")
	if err != nil {
		return "", fmt.Errorf("query balance: %w", err)
	}

	if all {
		// Everything above the reserve (and the fee). Computed from the live balance.
		if bal <= cfg.WithdrawReserve+fee {
			return "", fmt.Errorf("balance %s P2P is at or below the %s P2P reserve plus fee; nothing to withdraw", p2p(bal), p2p(cfg.WithdrawReserve))
		}
		amount = bal - fee - cfg.WithdrawReserve
	}

	if amount == 0 {
		return "", errors.New("amount must be positive")
	}
	if amount < cfg.WithdrawMin {
		return "", fmt.Errorf("amount %s P2P is below the minimum withdrawal of %s P2P", p2p(amount), p2p(cfg.WithdrawMin))
	}
	// Keep at least the reserve after amount + fee (overflow-safe ordering).
	if amount > bal || fee > bal-amount || cfg.WithdrawReserve > bal-amount-fee {
		return "", fmt.Errorf("would leave less than the %s P2P reserve: amount %s P2P (+%s fee), balance %s P2P", p2p(cfg.WithdrawReserve), p2p(amount), p2p(fee), p2p(bal))
	}

	num, seq, err := c.Account(ctx, acct.OperatorAddr())
	if err != nil {
		return "", fmt.Errorf("query account: %w", err)
	}

	memo := "SentNodes Agent Withdrawal"
	txBytes := chain.BuildSignedSend(acct, cfg.ChainID, cfg.WithdrawAddr, "udvpn", amount, fee, cfg.Gas, num, seq, memo)
	h, err := c.Broadcast(ctx, txBytes)
	if err != nil {
		return h, err
	}
	if werr := recordWithdraw(logPath, idemKey, h); werr != nil {
		log.Printf("WARNING: withdrawal %s broadcast but local record failed (%v); a retry relies on the on-chain sequence to avoid a double-send", h, werr)
	}
	return h, nil
}

func computeFee(gas uint64, gasPrices string) uint64 {
	price := parseGasPrice(gasPrices)
	if price <= 0 {
		price = 0.1
	}
	return uint64(math.Ceil(float64(gas) * price))
}

// p2p formats a udvpn amount as a P2P string (1 P2P = 1,000,000 udvpn).
func p2p(udvpn uint64) string {
	return strconv.FormatFloat(float64(udvpn)/1_000_000, 'f', -1, 64)
}

func parseGasPrice(s string) float64 {
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	v, _ := strconv.ParseFloat(s[:i], 64)
	return v
}

func lookupWithdraw(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == key {
			return f[1]
		}
	}
	return ""
}

func recordWithdraw(path, key, hash string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(key + " " + hash + "\n")
	return err
}

// tomlEdit is a single key whose value should be replaced within a given table.
// literal is the already-encoded TOML value text (e.g. `"foo"` or `["a", "b"]`).
type tomlEdit struct {
	table, key, literal string
}

// applyConfigEdit rewrites config.toml, preferring surgical text edits that preserve
// comments; if a key can't be located it falls back to a full marshal (valid, but drops
// comments). The write is atomic.
func applyConfigEdit(path string, edits []tomlEdit, mutate func(map[string]interface{}) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if text, ok := surgicalEdit(string(data), edits); ok {
		return atomicWrite(path, []byte(text))
	}
	var m map[string]interface{}
	if err := toml.Unmarshal(data, &m); err != nil {
		return err
	}
	if err := mutate(m); err != nil {
		return err
	}
	out, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWrite(path, out)
}

// atomicWrite writes data to a temp file in the same dir then renames over path,
// so a crash mid-write can never leave a truncated/corrupt config.toml.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// surgicalEdit applies every edit in order, returning the new text only if all
// keys were found and replaced; otherwise it reports false so the caller falls back.
func surgicalEdit(text string, edits []tomlEdit) (string, bool) {
	for _, e := range edits {
		next, ok := setKeyInTable(text, e.table, e.key, e.literal)
		if !ok {
			return "", false
		}
		text = next
	}
	return text, true
}

// setKeyInTable replaces key's value inside [table] with literal, leaving all other
// lines (comments, ordering) intact. It handles multi-line array values, and returns
// false if the key is absent, its value starts on a later line, or brackets are unbalanced.
func setKeyInTable(text, table, key, literal string) (string, bool) {
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	cur := ""
	for i := 0; i < len(lines); i++ {
		if name, ok := tableHeader(lines[i]); ok {
			cur = name
			continue
		}
		if cur != table || !isKeyLine(lines[i], key) {
			continue
		}
		// Require a value on this line (value-on-next-line isn't handled safely).
		if eq := strings.IndexByte(lines[i], '='); eq < 0 || strings.TrimSpace(lines[i][eq+1:]) == "" {
			return "", false
		}
		// Extend the span across a multi-line value (e.g. an array) if needed.
		end := i
		for depth := bracketDelta(lines[i]); depth > 0; {
			if end+1 >= len(lines) {
				return "", false // unterminated array
			}
			end++
			depth += bracketDelta(lines[end])
		}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
		out := make([]string, 0, len(lines))
		out = append(out, lines[:i]...)
		out = append(out, indent+key+" = "+literal)
		out = append(out, lines[end+1:]...)
		return strings.Join(out, nl), true
	}
	return "", false
}

// tableHeader returns the table name if line is a [table] or [[table]] header.
func tableHeader(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") {
		return "", false
	}
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "[["), "["), "]]"), "]")
	if name := strings.TrimSpace(s); name != "" {
		return name, true
	}
	return "", false
}

// isKeyLine reports whether line assigns the bare key (key = ...), ignoring
// leading whitespace.
func isKeyLine(line, key string) bool {
	s := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(s, key) {
		return false
	}
	return strings.HasPrefix(strings.TrimLeft(s[len(key):], " \t"), "=")
}

// bracketDelta is the net count of unclosed `[` on a line, ignoring brackets
// inside strings and after a comment.
func bracketDelta(line string) int {
	depth, inStr := 0, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr != 0 {
			if c == '\\' && inStr == '"' {
				i++ // skip the escaped char inside a basic string
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = c
		case '#':
			return depth
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth
}

// tomlString encodes s as a TOML basic string.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

// tomlStringArray encodes items as a TOML inline array of basic strings.
func tomlStringArray(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = tomlString(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
