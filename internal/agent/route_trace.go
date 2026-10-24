package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Bound network work even when multiple panel replicas issue commands.
var routeTraceSlots = make(chan struct{}, 2)

type traceOutput struct{ bytes.Buffer }

func (b *traceOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := 32*1024 - b.Len(); left > 0 {
		b.Buffer.Write(p[:min(left, len(p))])
	}
	return n, nil
}

type traceRunner func(context.Context, string, string) (string, error)

func runRouteTrace(ctx context.Context, family, target string) (string, error) {
	tool, err := exec.LookPath("traceroute")
	if err != nil {
		return "", exec.ErrNotFound
	}
	// Numeric output, one UDP probe per hop, no DNS or external ASN service.
	cmd := exec.CommandContext(ctx, tool, "-"+family, "-n", "-q", "1", "-w", "1", "-m", "20", target)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	var out traceOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
}

func collectRouteTrace(ctx context.Context, req model.RouteTraceRequest, run traceRunner) ([]model.RouteTraceResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	select {
	case routeTraceSlots <- struct{}{}:
		defer func() { <-routeTraceSlots }()
	default:
		return nil, errors.New("回程检测繁忙，请稍后重试")
	}
	results := make([]model.RouteTraceResult, 0, 2)
	for _, item := range []struct{ family, target string }{{"4", req.IPv4}, {"6", req.IPv6}} {
		if item.target != "" {
			results = append(results, model.RouteTraceResult{Family: "ipv" + item.family, Target: item.target})
		}
	}
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := &results[i]
			r.CheckedAt = time.Now().UTC()
			probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			raw, err := run(probeCtx, strings.TrimPrefix(r.Family, "ipv"), r.Target)
			if len(raw) > 32*1024 {
				raw = raw[:32*1024]
			}
			r.Raw = raw
			r.Hops, r.State = parseRouteTrace(raw, r.Target)
			switch {
			case errors.Is(err, exec.ErrNotFound):
				r.State = "unavailable"
			case probeCtx.Err() != nil:
				r.State = "timeout"
			case err != nil:
				r.State = "failed"
			}
		}(i)
	}
	wg.Wait()
	return results, nil
}

func parseRouteTrace(raw, target string) ([]model.RouteTraceHop, string) {
	hops := make([]model.RouteTraceHop, 0, 20)
	reached := false
	destination, _ := netip.ParseAddr(target)
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ttl, err := strconv.Atoi(fields[0])
		if err != nil || ttl < 1 || ttl > 20 {
			continue
		}
		h := model.RouteTraceHop{TTL: ttl, Addresses: []string{}}
		for _, field := range fields[1:] {
			ip, err := netip.ParseAddr(field)
			if err != nil {
				continue
			}
			h.Addresses = append(h.Addresses, ip.String())
			if ip == destination && !strings.Contains(line, "!") {
				reached = true
			}
		}
		hops = append(hops, h)
		if len(hops) == 20 {
			break
		}
	}
	if reached {
		return hops, "reached"
	}
	return hops, "partial"
}

func routeTraceCommand(ctx context.Context, raw string) (string, error) {
	var req model.RouteTraceRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return "", err
	}
	results, err := collectRouteTrace(ctx, req, runRouteTrace)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(results)
	return string(out), err
}
