package metrics

import (
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"

	"github.com/busurmedia/sentnodes-agent/internal/wire"
)

// Collect gathers host-level metrics. Disk usage is measured at root.
func Collect() wire.Metrics {
	var m wire.Metrics

	if c, err := cpu.Percent(150*time.Millisecond, false); err == nil && len(c) > 0 {
		m.CPUPct = round2(c[0])
	}
	if ci, err := cpu.Info(); err == nil && len(ci) > 0 {
		m.CPUModel = strings.Join(strings.Fields(ci[0].ModelName), " ") // collapse repeated spaces
	}
	if n, err := cpu.Counts(true); err == nil && n > 0 {
		m.CPUCores = n
	}
	if l, err := load.Avg(); err == nil && l != nil {
		m.LoadAvg1 = round2(l.Load1)
	}
	if vm, err := mem.VirtualMemory(); err == nil && vm != nil {
		m.MemUsed, m.MemTotal = vm.Used, vm.Total
	}
	if sw, err := mem.SwapMemory(); err == nil && sw != nil {
		m.SwapUsed, m.SwapTotal = sw.Used, sw.Total
	}
	if du, err := disk.Usage("/"); err == nil && du != nil {
		m.DiskUsed, m.DiskTotal = du.Used, du.Total
	}
	if io, err := net.IOCounters(false); err == nil && len(io) > 0 {
		m.NetRxBytes, m.NetTxBytes = io[0].BytesRecv, io[0].BytesSent
	}
	if hi, err := host.Info(); err == nil && hi != nil {
		m.UptimeSec = hi.Uptime
		m.OS = hi.Platform
		m.Kernel = hi.KernelVersion
		m.HostId = hi.HostID
	}
	return m
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// HostID reads the machine-id (no CPU sampling); set once at startup, then immutable.
func HostID() string {
	if hi, err := host.Info(); err == nil && hi != nil {
		return hi.HostID
	}
	return ""
}
