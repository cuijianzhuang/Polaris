package panel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const cdnAddressCheckBatch = 8

var errCDNScheduleChanged = errors.New("CDN check schedule changed")

func cdnAddressSchedule(p *store.CDNProfile) gin.H {
	r := p.AddressCheckRuntime.V
	return gin.H{"intervalMinutes": p.AddressCheckInterval, "nextAt": p.AddressCheckNextAt, "startedAt": r.StartedAt, "lastAt": r.LastAt, "finishedAt": r.FinishedAt, "checked": r.Checked, "total": r.Total, "failed": r.Failed, "partial": r.Partial, "error": r.Error,
		"running": p.AddressCheckLeaseID != "" && p.AddressCheckLeaseUntil != nil && p.AddressCheckLeaseUntil.After(time.Now())}
}

func (s *Server) updateCDNCheckSchedule(c *gin.Context) {
	var req struct {
		IntervalMinutes *int `json:"intervalMinutes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.IntervalMinutes == nil || (*req.IntervalMinutes != 0 && (*req.IntervalMinutes < 5 || *req.IntervalMinutes > 1440)) {
		badRequest(c, fmt.Errorf("检测周期须为 5–1440 分钟，0 为关闭"))
		return
	}
	var p store.CDNProfile
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		p.AddressCheckInterval = *req.IntervalMinutes
		p.AddressCheckNextAt = nil
		if p.AddressCheckInterval > 0 {
			now := time.Now()
			p.AddressCheckNextAt = &now
		}
		// Invalidating the token also prevents an in-flight old sweep from
		// writing verdicts after the operator disables or reconfigures it.
		p.AddressCheckLeaseID = ""
		p.AddressCheckLeaseUntil = nil
		p.AddressCheckRuntime = store.NewJSON(store.CDNAddressCheckRuntime{})
		return tx.Save(&p).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "cdn.address_check_schedule", p.Name, fmt.Sprintf("%d minutes", p.AddressCheckInterval))
	c.JSON(200, cdnAddressSchedule(&p))
}

type cdnScheduledTask struct {
	key             string
	context         cdnCheckContext
	address, origin string
}

func (s *Server) cdnScheduledTasks(p *store.CDNProfile) ([]cdnScheduledTask, string) {
	tasks := []cdnScheduledTask{}
	seen := map[string]bool{}
	notes := []string{}
	for _, entry := range s.cdnContexts(p) {
		var in store.Inbound
		if err := s.st.DB.First(&in, "id = ?", entry.InboundID).Error; err != nil {
			notes = append(notes, err.Error())
			continue
		}
		m := s.toModelInbound(&in)
		var ep store.Endpoint
		if err := s.st.DB.First(&ep, "id = ?", entry.EndpointID).Error; err != nil {
			notes = append(notes, err.Error())
			continue
		}
		// A broken entry configuration is not evidence that every address is
		// bad. Its warning remains visible, without poisoning address verdicts.
		if problem := s.cdnEndpointProblem(&m, in.ID, p, ep.Port); problem != "" {
			notes = append(notes, entry.Label+": "+problem)
			continue
		}
		var node store.Node
		s.st.DB.First(&node, "id = ?", in.NodeID)
		addresses := append(append([]string{}, s.cdnAddressesForPort(p, entry.Target.Port)...), p.PinnedHosts.V...)
		if entry.ExplicitAddress != "" || len(addresses) == 0 {
			addresses = append(addresses, entry.Target.Server)
		}
		for _, address := range addresses {
			if address == "" || cdnAddressBlocked(p, address) {
				continue
			}
			target := entry.Target
			target.Server = address
			key := cdnCheckKey(entry.EndpointID, target, entry.Transport, entry.Revision)
			if seen[key] {
				continue
			}
			seen[key] = true
			tasks = append(tasks, cdnScheduledTask{key: key, context: entry, address: address, origin: nodeOrigin(&node)})
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].key < tasks[j].key })
	return tasks, truncate(strings.Join(notes, "; "), 1000)
}

// Atomic database leases cover leader handover and a restarted panel. A lost
// worker releases its claim within three minutes without holding a DB tx open.
func (s *Server) claimCDNAddressCheck(ctx context.Context, id string, now time.Time) (store.CDNProfile, bool, error) {
	token := store.NewID()
	until := now.Add(3 * time.Minute)
	r := s.st.DB.WithContext(ctx).Model(&store.CDNProfile{}).Where("id = ? AND address_check_interval > 0", id).
		Where("address_check_next_at IS NULL OR address_check_next_at <= ?", now).
		Where("address_check_lease_until IS NULL OR address_check_lease_until <= ?", now).
		Updates(map[string]any{"address_check_lease_id": token, "address_check_lease_until": until})
	if r.Error != nil || r.RowsAffected == 0 {
		return store.CDNProfile{}, false, r.Error
	}
	var p store.CDNProfile
	err := s.st.DB.WithContext(ctx).First(&p, "id = ? AND address_check_lease_id = ?", id, token).Error
	return p, err == nil, err
}

func (s *Server) checkScheduledCDNAddresses() {
	if !s.cdnAddressCheckBusy.CompareAndSwap(false, true) {
		return
	}
	defer s.cdnAddressCheckBusy.Store(false)
	ctx, cancel := context.WithTimeout(s.ctxBG(), 3*time.Minute)
	defer cancel()
	var profiles []store.CDNProfile
	if err := s.st.DB.WithContext(ctx).Where("address_check_interval > 0").Where("address_check_next_at IS NULL OR address_check_next_at <= ?", time.Now()).Order("address_check_next_at ASC, id ASC").Find(&profiles).Error; err != nil {
		s.log.Warn("CDN scheduled checks", "err", err)
		return
	}
	for _, profile := range profiles {
		if ctx.Err() != nil {
			return
		}
		p, claimed, err := s.claimCDNAddressCheck(ctx, profile.ID, time.Now())
		if err != nil {
			s.log.Warn("CDN check claim", "profile", profile.ID, "err", err)
			continue
		}
		if !claimed {
			continue
		}
		if err = s.runCDNAddressCheckBatch(ctx, p); err != nil && !errors.Is(err, errCDNScheduleChanged) {
			s.log.Warn("CDN scheduled check", "profile", p.ID, "err", err)
		}
	}
}

func (s *Server) runCDNAddressCheckBatch(parent context.Context, p store.CDNProfile) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	tasks, note := s.cdnScheduledTasks(&p)
	keys := make([]string, len(tasks))
	for i, t := range tasks {
		keys[i] = t.key
	}
	b, _ := json.Marshal(keys)
	graph := fmt.Sprintf("%x", sha256.Sum256(b))
	run := p.AddressCheckRuntime.V
	if run.Cursor == "" || run.Graph != graph {
		now := time.Now()
		run = store.CDNAddressCheckRuntime{Graph: graph, StartedAt: &now}
	}
	run.Total = len(tasks)
	run.Error = note
	write := s.st.DB.WithContext(ctx).Model(&store.CDNProfile{}).Where("id = ? AND address_check_lease_id = ? AND address_check_interval > 0", p.ID, p.AddressCheckLeaseID).Update("address_check_runtime", store.NewJSON(run))
	if write.Error != nil {
		return write.Error
	}
	if write.RowsAffected == 0 {
		return errCDNScheduleChanged
	}
	processed := 0
	var failure error
	for _, task := range tasks {
		if task.key <= run.Cursor {
			continue
		}
		if processed >= cdnAddressCheckBatch || ctx.Err() != nil {
			break
		}
		target := task.context.Target
		target.Server = task.address
		result := cdnAddressResult{Target: target, EndpointID: task.context.EndpointID, CheckedAt: time.Now(), Viewpoint: "panel"}
		probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Second)
		result.cdnCheck = probeCDN(probeCtx, target, task.context.Transport, task.origin)
		probeCancel()
		if ctx.Err() != nil {
			break
		} // shutdown/batch timeout is not an address failure
		if err := s.saveCDNAddressResult(ctx, p.ID, task.key, result, p.AddressCheckLeaseID); err != nil {
			failure = err
			break
		}
		run.Cursor = task.key
		processed++
	}
	finishCtx, finishCancel := context.WithTimeout(s.ctxBG(), 5*time.Second)
	defer finishCancel()
	err := s.st.DB.WithContext(finishCtx).Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		var current store.CDNProfile
		if err := tx.First(&current, "id = ?", p.ID).Error; err != nil {
			return err
		}
		if current.AddressCheckLeaseID != p.AddressCheckLeaseID || current.AddressCheckInterval == 0 {
			return errCDNScheduleChanged
		}
		r := current.AddressCheckRuntime.V
		now := time.Now()
		next := now.Add(time.Minute)
		complete := failure == nil && ctx.Err() == nil && (len(tasks) == 0 || r.Cursor >= tasks[len(tasks)-1].key)
		if complete {
			r.Cursor = ""
			r.FinishedAt = &now
			next = now.Add(time.Duration(current.AddressCheckInterval) * time.Minute)
		}
		if failure != nil {
			r.Error = truncate(failure.Error(), 1000)
		}
		return tx.Model(&current).Updates(map[string]any{"address_check_runtime": store.NewJSON(r), "address_check_next_at": next, "address_check_lease_id": "", "address_check_lease_until": nil}).Error
	})
	if err != nil {
		return err
	}
	return failure
}
