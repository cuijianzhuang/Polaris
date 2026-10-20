// Package provider reads server status from hosting providers' APIs. It
// supports BandwagonHost (KiwiVM, also used by DMIT-style resellers on the
// same API): plan, monthly traffic used and allowance, next reset, and the
// live state of the VPS. Calls are read-only.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BWHBase is the KiwiVM API root (a variable for tests).
var BWHBase = "https://api.64clouds.com/v1"

// Info is a provider's view of one server.
type Info struct {
	Plan       string    `json:"plan"`
	Location   string    `json:"location"`
	OS         string    `json:"os"`
	Hostname   string    `json:"hostname"`
	IPs        []string  `json:"ips"`
	RAM        int64     `json:"ram"`  // bytes
	Disk       int64     `json:"disk"` // bytes
	DataUsed   int64     `json:"dataUsed"`
	DataLimit  int64     `json:"dataLimit"`
	Multiplier float64   `json:"multiplier"` // traffic accounting coefficient
	NextReset  time.Time `json:"nextReset"`
	Suspended  bool      `json:"suspended"`
	// Live state (LiveInfo only).
	Status    string    `json:"status,omitempty"` // running / stopped …
	DiskUsed  int64     `json:"diskUsed,omitempty"`
	MemAvail  int64     `json:"memAvail,omitempty"` // bytes
	Load      string    `json:"load,omitempty"`
	Throttled bool      `json:"throttled,omitempty"`
	Fetched   time.Time `json:"fetched"`
}

// BWH is one KiwiVM VPS.
type BWH struct {
	VEID   string
	APIKey string
	Client *http.Client
}

func (b *BWH) call(ctx context.Context, method string) (map[string]any, error) {
	if strings.TrimSpace(b.VEID) == "" || strings.TrimSpace(b.APIKey) == "" {
		return nil, errors.New("请填写 VEID 和 API Key")
	}
	q := url.Values{"veid": {strings.TrimSpace(b.VEID)}, "api_key": {strings.TrimSpace(b.APIKey)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BWHBase+"/"+method+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	cl := b.Client
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接搬瓦工 API 失败：%w", redactErr(err, b.APIKey))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("搬瓦工 API 返回了无法识别的内容（HTTP %d）", resp.StatusCode)
	}
	if code := num(m["error"]); code != 0 {
		msg, _ := m["message"].(string)
		return nil, fmt.Errorf("搬瓦工 API 错误 %d：%s", int(code), orDefault(msg, "未知错误"))
	}
	return m, nil
}

// redactErr keeps the API key out of error text (URLs end up in errors).
func redactErr(err error, key string) error {
	if key == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "***"))
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		var f float64
		fmt.Sscanf(x, "%g", &f)
		return f
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func parseService(m map[string]any) *Info {
	in := &Info{
		Plan: str(m["plan"]), Location: orDefault(str(m["node_location"]), str(m["node_datacenter"])), OS: str(m["os"]),
		Hostname: str(m["hostname"]), RAM: int64(num(m["plan_ram"])), Disk: int64(num(m["plan_disk"])),
		DataLimit: int64(num(m["plan_monthly_data"])), Multiplier: num(m["monthly_data_multiplier"]),
		Suspended: num(m["suspended"]) != 0, Fetched: time.Now(),
	}
	if in.Multiplier <= 0 {
		in.Multiplier = 1
	}
	// data_counter is raw traffic; the allowance is charged at the multiplier.
	in.DataUsed = int64(num(m["data_counter"]) * in.Multiplier)
	if t := int64(num(m["data_next_reset"])); t > 0 {
		in.NextReset = time.Unix(t, 0)
	}
	if ips, ok := m["ip_addresses"].([]any); ok {
		for _, ip := range ips {
			if s := str(ip); s != "" {
				in.IPs = append(in.IPs, s)
			}
		}
	}
	return in
}

// ServiceInfo reads the plan and traffic (cheap; safe to poll).
func (b *BWH) ServiceInfo(ctx context.Context) (*Info, error) {
	m, err := b.call(ctx, "getServiceInfo")
	if err != nil {
		return nil, err
	}
	return parseService(m), nil
}

// LiveInfo also reads the VPS's live state (slower; on demand).
func (b *BWH) LiveInfo(ctx context.Context) (*Info, error) {
	m, err := b.call(ctx, "getLiveServiceInfo")
	if err != nil {
		return nil, err
	}
	in := parseService(m)
	in.Status = orDefault(str(m["ve_status"]), str(m["vz_status"]))
	if st, ok := m["vz_status"].(map[string]any); ok { // OpenVZ: an object
		in.Status = str(st["status"])
	}
	in.DiskUsed = int64(num(m["ve_used_disk_space_b"]))
	in.MemAvail = int64(num(m["mem_available_kb"]) * 1024)
	in.Load = str(m["load_average"])
	in.Throttled = num(m["is_cpu_throttled"]) != 0
	return in, nil
}
