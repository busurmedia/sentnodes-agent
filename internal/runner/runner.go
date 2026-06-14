// Package runner wires the agent together: preflight, enrollment, the command
// poll loop, periodic metrics, and event-driven status heartbeats.
package runner

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/busurmedia/sentnodes-agent/internal/api"
	"github.com/busurmedia/sentnodes-agent/internal/bech32"
	"github.com/busurmedia/sentnodes-agent/internal/check"
	"github.com/busurmedia/sentnodes-agent/internal/config"
	"github.com/busurmedia/sentnodes-agent/internal/dockerx"
	"github.com/busurmedia/sentnodes-agent/internal/exec"
	"github.com/busurmedia/sentnodes-agent/internal/keys"
	"github.com/busurmedia/sentnodes-agent/internal/metrics"
	"github.com/busurmedia/sentnodes-agent/internal/wire"
)

type Runner struct {
	cfg     *config.Config
	cli     *api.Client
	dk      *dockerx.Docker
	acct    *keys.Account
	token   string
	version string

	containerID   string
	containerName string
	signals       check.Signals
	hostID        string
	loggedLatest  string
}

func Run(ctx context.Context, version string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	r := &Runner{cfg: cfg, cli: api.New(cfg.ServerURL), version: version}
	r.hostID = metrics.HostID() // set once, before any goroutine reads it
	// Writability is soft: a read-only home marks Volume/Config unhealthy but doesn't stop
	// the agent. A temp-write+rename means directory write access is what actually matters.
	configReadable := fileReadable(cfg.ConfigPath())
	configValid := cfg.FromName != "" && len(cfg.RPCAddrs) > 0
	homeWritable := dirWritable(cfg.NodeHome)
	r.signals.Volume = configReadable && homeWritable
	r.signals.Config = configValid && homeWritable
	r.signals.Backend = cfg.KeyringBackend == "test"

	// Invalid withdrawal address: disable withdrawals so a typo isn't reported as enabled.
	if r.cfg.WithdrawAddr != "" && !bech32.IsValid(r.cfg.WithdrawAddr, "sent") {
		log.Printf("WITHDRAWAL_ADDRESS %q is not a valid sent1 address; withdrawals disabled", r.cfg.WithdrawAddr)
		r.cfg.WithdrawAddr = ""
	}

	// Enforce the hard floor on both limits.
	floorP2P := config.WithdrawFloorUdvpn / config.UdvpnPerP2P
	if r.cfg.WithdrawMin < config.WithdrawFloorUdvpn {
		log.Printf("WITHDRAWAL_MIN raised to the %d P2P floor to prevent spam transactions", floorP2P)
		r.cfg.WithdrawMin = config.WithdrawFloorUdvpn
	}
	if r.cfg.WithdrawReserve < config.WithdrawFloorUdvpn {
		log.Printf("WITHDRAWAL_RESERVE raised to the %d P2P floor to ensure sufficient funds for node operations", floorP2P)
		r.cfg.WithdrawReserve = config.WithdrawFloorUdvpn
	}

	// Hard prerequisite: load the operator key (also enforces backend=test).
	r.acct, err = keys.Open(cfg.NodeHome, cfg.KeyringBackend, cfg.FromName, "sent", "sentnode")
	if err != nil {
		return err
	}
	r.signals.Keyring = true

	// Hard prerequisites, checked individually so the failure reason is explicit.
	// Writability is excluded on purpose — it is a soft signal (Volume/Config).
	if !configReadable {
		return errf("preflight failed: config.toml not readable at " + cfg.ConfigPath())
	}
	if !configValid {
		return errf("preflight failed: config.toml missing required fields (keyring from-name and at least one RPC endpoint)")
	}
	if !r.signals.Backend {
		return errf("preflight failed: keyring backend must be \"test\"")
	}

	// Soft: Docker + container discovery.
	if dk, derr := dockerx.New(); derr == nil {
		r.dk = dk
		if id, name, e := dk.Discover(ctx, cfg.NodeHome, cfg.NodeContainer); e == nil {
			r.containerID, r.containerName, r.signals.Container = id, name, true
		} else {
			log.Printf("container discovery: %v", e)
		}
	} else {
		log.Printf("docker unavailable: %v", derr)
	}

	if err := r.ensureEnrolled(ctx); err != nil {
		return nil // context canceled while waiting to register
	}
	r.signals.Server = true
	log.Printf("enrolled node %s (operator %s)", r.acct.NodeAddr(), r.acct.OperatorAddr())
	log.Printf("SentNodes Agent %s successfully started", r.version)

	return r.loop(ctx)
}

// ensureEnrolled registers, retrying every 10m until it succeeds. A fresh node
// may not be indexed by SentNodes yet (NODE_UNKNOWN), so retry instead of crash-looping.
func (r *Runner) ensureEnrolled(ctx context.Context) error {
	for {
		err := r.enroll()
		if err == nil {
			return nil
		}
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "NODE_UNKNOWN" {
			log.Printf("node %s is not indexed by SentNodes yet, retrying registration in 10m", r.acct.NodeAddr())
		} else {
			log.Printf("registration failed: %v; retrying in 10m", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Minute):
		}
	}
}

func isAuthErr(err error) bool {
	var ae *api.APIError
	return errors.As(err, &ae) && ae.Status == 401
}

// reenroll discards a rejected token and re-registers (blocks until success/cancel).
func (r *Runner) reenroll(ctx context.Context) {
	log.Printf("agent token rejected; re-registering")
	_ = os.Remove(r.cfg.StatePath())
	r.token = ""
	_ = r.ensureEnrolled(ctx)
}

func (r *Runner) enroll() error {
	addr, t := readState(r.cfg.StatePath())
	if t != "" {
		switch {
		case addr == "":
			// Legacy token (no bound node address): adopt it for the current node and
			// upgrade the state file in place - no re-register needed.
			r.token = t
			_ = writeState(r.cfg.StatePath(), r.acct.NodeAddr(), t)
			return nil
		case addr == r.acct.NodeAddr():
			r.token = t
			return nil
		default:
			// The operator key changed, so the node address changed under us - the
			// stored token belongs to a different node. Re-enroll as the new node.
			log.Printf("node identity changed (%s -> %s); re-registering", addr, r.acct.NodeAddr())
		}
	}
	ch, err := r.cli.Challenge(r.acct.NodeAddr(), r.cfg.APIKey)
	if err != nil {
		return err
	}
	reg, err := r.cli.Register(r.cfg.APIKey, wire.RegisterReq{
		NodeAddr:            r.acct.NodeAddr(),
		OperatorAddr:        r.acct.OperatorAddr(),
		Pubkey:              r.acct.PubKeyHex(),
		Signature:           r.acct.SignHex([]byte(ch.Nonce)),
		HostId:              r.hostID,
		NodeContainer:       r.containerName,
		Version:             r.version,
		WithdrawDestination: r.cfg.WithdrawAddr,
		WithdrawMin:         r.cfg.WithdrawMin,
		WithdrawReserve:     r.cfg.WithdrawReserve,
	})
	if err != nil {
		return err
	}
	r.token = reg.AgentToken
	return writeState(r.cfg.StatePath(), r.acct.NodeAddr(), r.token)
}

func (r *Runner) loop(ctx context.Context) error {
	pollT := time.NewTicker(config.PollInterval)
	hbT := time.NewTicker(config.HeartbeatInterval)
	metricsT := time.NewTicker(config.MetricsInterval)
	defer pollT.Stop()
	defer hbT.Stop()
	defer metricsT.Stop()

	go r.watchEvents(ctx)

	// Poll first: it re-enrolls on a rejected token, so the heartbeat + metrics
	// below use a valid token and populate immediately instead of lagging a tick.
	r.pollOnce(ctx)
	r.heartbeat(ctx)
	r.sendMetrics(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollT.C:
			r.pollOnce(ctx)
		case <-hbT.C:
			r.heartbeat(ctx)
		case <-metricsT.C:
			r.sendMetrics(ctx)
		}
	}
}

func (r *Runner) pollOnce(ctx context.Context) {
	cmds, latest, err := r.cli.Poll(r.token)
	if err != nil {
		if isAuthErr(err) {
			r.reenroll(ctx) // token revoked/expired: drop it and register again
			return
		}
		log.Printf("poll: %v", err)
		return
	}
	if latest != "" && latest != r.version && latest != r.loggedLatest {
		log.Printf("update available: %s (running %s) - recreate the container, or run Watchtower to auto-upgrade", latest, r.version)
		r.loggedLatest = latest
	}
	// Apply every config edit first, then restart once - a batch of price/rpc/moniker/
	// restart commands costs a single restart, not one per command. Config commands
	// whose edit succeeds (and any restart command) share that restart's result.
	restart := false
	var deferred []wire.Command
	for _, c := range cmds {
		log.Printf("command %s: processing %s", c.ID, c.Type)
		switch c.Type {
		case "restart":
			restart = true
			deferred = append(deferred, c)
		case "price_update", "rpc_update", "moniker_update":
			if ok, result := r.applyConfig(c); ok {
				restart = true
				deferred = append(deferred, c)
			} else {
				r.finish(c, false, result)
			}
		default:
			ok, result := r.execute(ctx, c)
			r.finish(c, ok, result)
		}
	}
	if restart {
		ok, result := r.restartNode(ctx)
		for _, c := range deferred {
			r.finish(c, ok, result)
		}
	}
}

func (r *Runner) finish(c wire.Command, ok bool, result map[string]interface{}) {
	if ok {
		log.Printf("command %s: %s succeeded", c.ID, c.Type)
	} else {
		log.Printf("command %s: %s failed: %v", c.ID, c.Type, result["error"])
	}
	if err := r.cli.Result(r.token, c.ID, ok, result); err != nil {
		log.Printf("result %s: %v", c.ID, err)
	}
}

// applyConfig writes a config command's changes without restarting; the poll loop
// restarts once after every edit in the batch is applied.
func (r *Runner) applyConfig(c wire.Command) (bool, map[string]interface{}) {
	switch c.Type {
	case "price_update":
		gb, _ := c.Payload["gigabytePrices"].(string)
		hr, _ := c.Payload["hourlyPrices"].(string)
		if err := exec.EditPrices(r.cfg, gb, hr); err != nil {
			return false, fail(err.Error())
		}
		if ov, ok := c.Payload["oracle"]; ok { // oracle is optional
			oracle, _ := ov.(string)
			if err := exec.EditOracle(r.cfg, oracle); err != nil {
				return false, fail(err.Error())
			}
		}
		return true, nil
	case "rpc_update":
		addrs := toStrings(c.Payload["addrs"])
		if len(addrs) == 0 {
			return false, fail("no rpc addresses provided")
		}
		if err := exec.EditRPC(r.cfg, addrs); err != nil {
			return false, fail(err.Error())
		}
		return true, nil
	case "moniker_update":
		moniker, _ := c.Payload["moniker"].(string)
		if err := exec.EditMoniker(r.cfg, moniker); err != nil {
			return false, fail(err.Error())
		}
		return true, nil
	}
	return false, fail("unknown config command: " + c.Type)
}

func (r *Runner) execute(ctx context.Context, c wire.Command) (bool, map[string]interface{}) {
	switch c.Type {
	case "status":
		return true, exec.Status(r.cfg, r.acct)
	case "withdraw":
		all, _ := c.Payload["all"].(bool)
		amt, _ := toUint(c.Payload["amount"])
		if !all && amt == 0 {
			return false, fail("invalid withdrawal amount")
		}
		h, err := exec.Withdraw(ctx, r.cfg, r.acct, amt, all, c.ID)
		if err != nil {
			return false, fail(err.Error())
		}
		return true, map[string]interface{}{"txHash": h}
	default:
		return false, fail("unknown command type: " + c.Type)
	}
}

func (r *Runner) restartNode(ctx context.Context) (bool, map[string]interface{}) {
	if r.containerID == "" {
		return false, fail("node container not found")
	}
	if err := r.dk.Restart(ctx, r.containerID); err != nil {
		return false, fail(err.Error())
	}
	return true, nil
}

func (r *Runner) sendMetrics(ctx context.Context) {
	m := metrics.Collect()
	m.HostId = r.hostID // immutable, set at startup
	if r.dk != nil && r.containerID != "" {
		cpu, mem := r.dk.Stats(ctx, r.containerID)
		m.NodeCPUPct = round2(cpu)
		m.NodeMemUsed = mem
	}
	if err := r.cli.Metrics(r.token, m); err != nil && !isAuthErr(err) {
		log.Printf("metrics: %v", err) // a 401 is handled (and logged once) by the poll loop
	}
}

func (r *Runner) heartbeat(ctx context.Context) {
	moniker, gb, hr, oracle, rpc := r.cfg.NodeConfigSnapshot() // live, so it reflects edits the agent applied
	hb := wire.Heartbeat{
		Version:             r.version,
		NodeContainer:       r.containerName,
		WithdrawDestination: r.cfg.WithdrawAddr,
		WithdrawMin:         r.cfg.WithdrawMin,
		WithdrawReserve:     r.cfg.WithdrawReserve,
		Moniker:             moniker,
		GigabytePrices:      gb,
		HourlyPrices:        hr,
		Oracle:              oracle,
		RPCAddrs:            rpc,
		HostId:              r.hostID,
		HealthChecks:        r.signals.Map(),
	}
	if r.dk != nil && r.containerID != "" {
		if st, err := r.dk.Inspect(ctx, r.containerID); err == nil {
			hb.NodeState = st.Status
			hb.NodeHealth = st.Health
			ec, rc := st.ExitCode, st.RestartCount
			hb.NodeExitCode = &ec
			hb.NodeRestartCount = &rc
			hb.NodeStartedAt = st.StartedAt
		}
	}
	if err := r.cli.Heartbeat(r.token, hb); err != nil && !isAuthErr(err) {
		log.Printf("heartbeat: %v", err) // a 401 is handled (and logged once) by the poll loop
	}
}

func (r *Runner) watchEvents(ctx context.Context) {
	if r.dk == nil || r.containerID == "" {
		return
	}
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	for ctx.Err() == nil {
		ch := r.dk.Events(ctx, r.containerID)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					time.Sleep(5 * time.Second) // stream ended; reconnect
					goto reconnect
				}
				debounce.Reset(2 * time.Second) // collapse stop+die+start into the settled state
			case <-debounce.C:
				r.heartbeat(ctx)
			}
		}
	reconnect:
	}
}

// --- helpers ---

func fail(msg string) map[string]interface{} { return map[string]interface{}{"error": msg} }

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func toUint(v interface{}) (uint64, bool) {
	if f, ok := v.(float64); ok && f >= 0 {
		return uint64(f), true
	}
	return 0, false
}

func toStrings(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func fileReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// dirWritable probes whether files can be created in dir (what the temp-write+rename
// config edit needs) by creating and removing a temp file. The config.toml file mode is
// irrelevant - rename only needs directory write access.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".sentagent-wcheck-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// readState returns the (nodeAddr, token) the agent last enrolled with; a node
// address change (operator re-key) is detected and triggers re-enrollment.
func readState(path string) (addr, token string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	switch f := strings.Fields(string(b)); len(f) {
	case 1:
		return "", f[0]
	case 2:
		return f[0], f[1]
	default:
		return "", ""
	}
}

func writeState(path, addr, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(addr+" "+token), 0o600)
}

func errf(s string) error { return errString(s) }

type errString string

func (e errString) Error() string { return string(e) }
