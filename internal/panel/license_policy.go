package panel

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	licenseCheckInterval = 6 * time.Hour
	licenseCheckJitter   = 30 * time.Minute
	licenseCheckCooldown = 30 * time.Second
)

type licenseCheckSchedule struct {
	lastAttempt time.Time
	next        time.Time
	failures    int
}

type licenseCheckWait struct{ seconds int }

func (e *licenseCheckWait) Error() string {
	return fmt.Sprintf("许可证校验过于频繁，请 %d 秒后重试", e.seconds)
}

func nextLicenseCheckDelay(failures int) time.Duration {
	switch failures {
	case 0:
		return licenseCheckInterval + time.Duration(rand.Int64N(int64(licenseCheckJitter)))
	case 1:
		return 5 * time.Minute
	case 2:
		return 30 * time.Minute
	default:
		return 2 * time.Hour
	}
}

// attemptLicenseCheckLocked is shared by manual and automatic attempts, so
// manual success cancels an old retry and failures never create parallel loops.
func (s *Server) attemptLicenseCheckLocked(ctx context.Context) error {
	now := time.Now()
	s.licenseSchedule.lastAttempt = now
	s.licenseManualAvailable.Store(now.Add(licenseCheckCooldown).UnixNano())
	err := s.checkLicenseLocked(ctx)
	if err != nil {
		s.licenseSchedule.failures = min(s.licenseSchedule.failures+1, 3)
	} else {
		s.licenseSchedule.failures = 0
	}
	s.licenseSchedule.next = time.Now().Add(nextLicenseCheckDelay(s.licenseSchedule.failures))
	return err
}

func (s *Server) performLicenseCheck(ctx context.Context, manual bool) error {
	if !s.licenseMu.TryLock() {
		return &licenseCheckWait{seconds: 2}
	}
	defer s.licenseMu.Unlock()
	l := s.installedLicense()
	if l == nil || l.Offline {
		return nil
	}
	if manual {
		if left := time.Until(s.licenseSchedule.lastAttempt.Add(licenseCheckCooldown)); left > 0 {
			return &licenseCheckWait{seconds: int((left + time.Second - 1) / time.Second)}
		}
	}
	return s.attemptLicenseCheckLocked(ctx)
}

// licenseCheckTick runs immediately on startup, then waits for the common
// schedule. A short upper bound lets a newly imported license join the loop.
func (s *Server) licenseCheckTick(ctx context.Context) time.Duration {
	if !s.licenseMu.TryLock() {
		return time.Second
	}
	defer s.licenseMu.Unlock()
	// CLI installs write to the same database without sharing our memory.
	if p := s.lic.Load(); p == nil || p.text != s.st.GetSetting("license_text", "") {
		s.reloadLicense()
		s.licenseSchedule.next = time.Time{}
	}
	l := s.installedLicense()
	if l == nil || l.Offline {
		return time.Minute
	}
	if !time.Now().Before(s.licenseSchedule.next) {
		_ = s.attemptLicenseCheckLocked(ctx)
	}
	return max(time.Second, min(time.Until(s.licenseSchedule.next), time.Minute))
}

func (s *Server) runLicenseChecks(ctx context.Context) {
	for ctx.Err() == nil {
		delay := s.licenseCheckTick(ctx)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

const licenseReadOnlyMessage = "超过 14 天未成功校验许可证，管理修改已暂停；已有代理继续运行，请联网校验或导入离线许可证"

// Only the authenticated admin REST group uses this guard. Agent RPC,
// subscriptions, user self-service and background reconciliation keep running.
func (s *Server) licenseManagementGuard(c *gin.Context) {
	if !s.license().ManagementReadOnly || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
		c.Next()
		return
	}
	route := strings.TrimPrefix(c.FullPath(), s.cfg.APIBase())
	// Keep existing authentication and authorization on every recovery route.
	if strings.HasPrefix(route, "/auth/") || route == "/license" || strings.HasPrefix(route, "/license/") ||
		route == "/system/update" || (c.Request.Method == http.MethodDelete && route == "/tokens/:id") {
		c.Next()
		return
	}
	c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": licenseReadOnlyMessage, "licenseReadOnly": true})
}

// Admin callback groups contain both views and mutations; deny every
// non-view action so new write actions cannot accidentally bypass the policy.
func tgLicenseManagementWrite(parts []string) bool {
	if len(parts) < 2 {
		return false
	}
	switch parts[0] {
	case "n":
		return parts[1] != "l" && parts[1] != "v"
	case "u":
		return parts[1] != "v"
	case "up":
		return true
	default:
		return false
	}
}
