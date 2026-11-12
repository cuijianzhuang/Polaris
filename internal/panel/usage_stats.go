package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anonymous usage statistics: how many Polaris instances are in use.
//
// Once a day this panel sends one ping to the license server — only its
// random instance id, its version, and its edition ("pro" or "community").
// Nothing about its users, nodes, IP addresses or URLs is ever sent, and
// the server stores one row per instance (no IPs). The ping is on by
// default; the admin can turn it off in Settings → 常规 → 使用统计
// (setting "usage_stats" = "false"). The same server also answers
// GET /usage.json, which the dashboard reads to show the global
// 30-day active instance count next to this panel's own numbers.

const usageStatsSetting = "usage_stats"

// usageCountsTTL bounds how long the global count is trusted: the
// dashboard polls statsOverview repeatedly, and the license server is
// only asked again after this long.
const usageCountsTTL = 10 * time.Minute

// usageCounts is the license server's GET /usage.json document.
type usageCounts struct {
	Instances30d int `json:"instances_30d"`
	Instances7d  int `json:"instances_7d"`
	Pro30d       int `json:"pro_30d"`
	Community30d int `json:"community_30d"`
}

// usageStatsEnabled reports whether the daily anonymous ping is on.
func (s *Server) usageStatsEnabled() bool {
	return s.st.GetSetting(usageStatsSetting, "true") != "false"
}

// usageEdition is the edition this panel reports.
func (s *Server) usageEdition() string {
	if proBuild {
		return "pro"
	}
	return "community"
}

var usageHTTPClient = &http.Client{Timeout: 10 * time.Second}

// postUsagePing sends one anonymous usage ping. It never returns a
// transport error to a caller that would surface it: the ping is
// best-effort and must never break the panel.
func postUsagePing(ctx context.Context, client *http.Client, base, instance, version, edition string) error {
	body, err := json.Marshal(map[string]string{"instance": instance, "version": version, "edition": edition})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/usage", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "polaris-panel/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("usage ping rejected: %s", resp.Status)
	}
	return nil
}

// reportUsageStats is the daily job: one anonymous ping, silently skipped
// when telemetry is off or no license server is configured, and never
// fatal when the server is unreachable.
func (s *Server) reportUsageStats() {
	if !s.usageStatsEnabled() {
		return
	}
	base := s.licenseServer()
	if base == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := postUsagePing(ctx, usageHTTPClient, base, s.instanceID(), Version, s.usageEdition()); err != nil {
		s.log.Warn("usage stats ping failed", "err", err)
	}
}

// fetchUsageCounts reads the license server's public usage counts.
func fetchUsageCounts(ctx context.Context, client *http.Client, base string) (usageCounts, error) {
	var c usageCounts
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/usage.json", nil)
	if err != nil {
		return c, err
	}
	req.Header.Set("User-Agent", "polaris-panel/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("usage counts rejected: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&c); err != nil {
		return c, err
	}
	return c, nil
}

// globalUsageCounts returns the license server's 30-day active instance
// count for the dashboard. Best-effort: it reports false when no license
// server is configured or the server cannot be reached, and the dashboard
// then hides the card. Successful fetches are cached for usageCountsTTL.
func (s *Server) globalUsageCounts() (usageCounts, bool) {
	base := s.licenseServer()
	if base == "" {
		return usageCounts{}, false
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if time.Since(s.usageCachedAt) < usageCountsTTL {
		return s.usageCached, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := fetchUsageCounts(ctx, usageHTTPClient, base)
	if err != nil {
		s.log.Warn("usage counts fetch failed", "err", err)
		return usageCounts{}, false
	}
	s.usageCached, s.usageCachedAt = c, time.Now()
	return c, true
}
