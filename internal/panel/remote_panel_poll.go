package panel

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

const (
	remotePanelPollInterval  = 60 * time.Second
	remotePanelTimeout       = 10 * time.Second
	remotePanelJitter        = 5 * time.Second
	remotePanelMaxBackoff    = 30 * time.Minute
	remotePanelMaxConcurrent = 8
)

// runRemotePanelPoller refreshes snapshots once per interval. It runs on
// every instance but only the leader actually polls, so instances sharing a
// database do not duplicate requests to members.
func (s *Server) runRemotePanelPoller(ctx context.Context) {
	ticker := time.NewTicker(remotePanelPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			time.Sleep(time.Duration(rand.Int63n(int64(remotePanelJitter))))
			if s.isLeader() {
				s.pollRemotePanels(ctx)
			}
		}
	}
}

// pollRemotePanels loads enabled panels and refreshes each one, bounded by a
// semaphore so a large registry does not burst outbound connections.
func (s *Server) pollRemotePanels(ctx context.Context) {
	var panels []store.RemotePanel
	if err := s.st.DB.Where("enabled = ?", true).Find(&panels).Error; err != nil {
		s.log.Warn("list remote panels failed", "error", err)
		return
	}
	if len(panels) == 0 {
		return
	}

	sem := make(chan struct{}, remotePanelMaxConcurrent)
	for i := range panels {
		p := panels[i]
		if p.NextTryAt != nil && time.Now().Before(*p.NextTryAt) {
			continue
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				s.pollOneRemotePanel(ctx, &p)
			}()
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) pollOneRemotePanel(ctx context.Context, p *store.RemotePanel) {
	token, err := s.st.Box.Open(p.Token)
	if err != nil {
		s.recordRemotePanelFailure(p, "decrypt_token")
		return
	}

	c := &remotePanelClient{
		baseURL:  p.BaseURL,
		token:    token,
		caPin:    p.CAPin,
		insecure: p.Insecure,
		timeout:  remotePanelTimeout,
		rt:       s.remotePanelTransport,
	}

	snap, err := c.fetchSnapshot(ctx)
	if err != nil {
		s.recordRemotePanelFailure(p, classifyRemotePanelError(err))
		return
	}

	now := time.Now()
	p.Snapshot = store.JSON[store.RemoteSnapshot]{V: *snap}
	p.SnapshotAt = &now
	p.LastOKAt = &now
	p.Failures = 0
	p.NextTryAt = nil

	if err := s.st.DB.Save(p).Error; err != nil {
		s.log.Warn("save remote panel snapshot", "panel", p.Name, "error", err)
	}
}

func (s *Server) recordRemotePanelFailure(p *store.RemotePanel, reason string) {
	p.Failures++
	p.LastError = reason
	backoff := time.Duration(p.Failures*p.Failures) * remotePanelPollInterval
	if backoff > remotePanelMaxBackoff {
		backoff = remotePanelMaxBackoff
	}
	next := time.Now().Add(backoff)
	p.NextTryAt = &next

	if err := s.st.DB.Save(p).Error; err != nil {
		s.log.Warn("save remote panel failure", "panel", p.Name, "error", err)
	}
	s.log.Warn("remote panel poll failed", "panel", p.Name, "reason", reason, "failures", p.Failures, "next_try", next.Format(time.RFC3339))
}

// classifyRemotePanelError maps a transport error to a fixed reason code so
// hub logs and UI never carry the member's raw internal text.
func classifyRemotePanelError(err error) string {
	if err == nil {
		return ""
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return "timeout"
	case strings.Contains(s, "tls") || strings.Contains(s, "certificate") || strings.Contains(s, "x509"):
		return "tls"
	case strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "unauthorized"):
		return "unauthorized"
	case strings.Contains(s, "500") || strings.Contains(s, "502") || strings.Contains(s, "503"):
		return "http_5xx"
	case strings.Contains(s, "decode") || strings.Contains(s, "unmarshal"):
		return "schema"
	case strings.Contains(s, "loopback") || strings.Contains(s, "link-local") || strings.Contains(s, "metadata"):
		return "blocked_target"
	default:
		return "network"
	}
}
