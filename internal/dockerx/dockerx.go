// Package dockerx talks to the Docker Engine API directly (no SDK dependency) to
// discover, inspect, restart, watch events for, and read stats of the node container.
package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Docker struct {
	http *http.Client
	base string
}

func New() (*Docker, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	// tcp:// is used when fronting the daemon with a socket-proxy.
	if strings.HasPrefix(host, "tcp://") {
		return &Docker{http: &http.Client{Timeout: 30 * time.Second}, base: "http://" + strings.TrimPrefix(host, "tcp://")}, nil
	}
	if strings.HasPrefix(host, "unix://") {
		socket := strings.TrimPrefix(host, "unix://")
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}
		return &Docker{http: &http.Client{Transport: transport, Timeout: 30 * time.Second}, base: "http://docker"}, nil
	}
	return nil, fmt.Errorf("unsupported DOCKER_HOST %q (use unix:// or tcp://)", host)
}

type State struct {
	ID           string
	Name         string
	Status       string
	Health       string
	ExitCode     int
	StartedAt    string
	RestartCount int
}

type containerJSON struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State *struct {
		Status    string `json:"Status"`
		ExitCode  int    `json:"ExitCode"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	Mounts       []struct {
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// Discover finds the node container by the shared volume (or preferName).
func (d *Docker) Discover(ctx context.Context, nodeHome, preferName string) (string, string, error) {
	if preferName != "" {
		c, err := d.inspect(ctx, preferName)
		if err != nil {
			return "", "", err
		}
		return c.ID, strings.TrimPrefix(c.Name, "/"), nil
	}

	selfSource, selfName := d.selfMount(ctx, nodeHome)

	var list []struct {
		ID string `json:"Id"`
	}
	if err := d.getJSON(ctx, "/containers/json?all=1", &list); err != nil {
		return "", "", err
	}
	self, _ := os.Hostname()
	for _, item := range list {
		if strings.HasPrefix(item.ID, self) {
			continue
		}
		c, err := d.inspect(ctx, item.ID)
		if err != nil {
			continue
		}
		for _, m := range c.Mounts {
			matchesVolume := selfSource != "" && (m.Source == selfSource || (selfName != "" && m.Name == selfName))
			looksLikeNode := strings.Contains(m.Destination, ".sentinel")
			if matchesVolume || looksLikeNode {
				return c.ID, strings.TrimPrefix(c.Name, "/"), nil
			}
		}
	}
	return "", "", fmt.Errorf("node container not found (set DVPN_NODE_CONTAINER to disambiguate)")
}

func (d *Docker) selfMount(ctx context.Context, nodeHome string) (source, name string) {
	self, err := os.Hostname()
	if err != nil {
		return "", ""
	}
	c, err := d.inspect(ctx, self)
	if err != nil {
		return "", ""
	}
	for _, m := range c.Mounts {
		if m.Destination == nodeHome {
			return m.Source, m.Name
		}
	}
	return "", ""
}

func (d *Docker) Inspect(ctx context.Context, id string) (*State, error) {
	c, err := d.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	s := &State{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), RestartCount: c.RestartCount}
	if c.State != nil {
		s.Status = c.State.Status
		s.ExitCode = c.State.ExitCode
		s.StartedAt = c.State.StartedAt
		if c.State.Health != nil {
			s.Health = c.State.Health.Status
		}
	}
	return s, nil
}

func (d *Docker) Restart(ctx context.Context, id string) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", d.base+"/containers/"+url.PathEscape(id)+"/restart", nil)
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restart failed: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

type statsJSON struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  uint32 `json:"online_cpus"`
}

// Stats returns the node container's CPU percent and memory used (bytes).
func (d *Docker) Stats(ctx context.Context, id string) (cpuPct float64, memUsed uint64) {
	var s statsJSON
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", &s); err != nil {
		return 0, 0
	}
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if sysDelta > 0 && cpuDelta > 0 && cpus > 0 {
		cpuPct = (cpuDelta / sysDelta) * cpus * 100
	}
	memUsed = s.MemoryStats.Usage
	if v, ok := s.MemoryStats.Stats["cache"]; ok && memUsed >= v {
		memUsed -= v
	}
	return cpuPct, memUsed
}

// Events streams container events for id; each value is a state-change signal.
func (d *Docker) Events(ctx context.Context, id string) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		filter := fmt.Sprintf(`{"type":["container"],"container":[%q]}`, id)
		req, _ := http.NewRequestWithContext(ctx, "GET", d.base+"/events?filters="+url.QueryEscape(filter), nil)
		// No client timeout for the streaming connection.
		streamClient := &http.Client{Transport: d.http.Transport}
		resp, err := streamClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var msg json.RawMessage
			if err := dec.Decode(&msg); err != nil {
				return
			}
			select {
			case out <- struct{}{}:
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	return out
}

func (d *Docker) inspect(ctx context.Context, id string) (*containerJSON, error) {
	var c containerJSON
	if err := d.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *Docker) getJSON(ctx context.Context, path string, out interface{}) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", d.base+path, nil)
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
