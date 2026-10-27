package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const notificationPolicyKey = "notification_policy"

var errNotificationNodes = errors.New("维护窗口包含不存在的节点")

var notificationChannels = []string{"telegram", "email", "webhook"}

// Only operational events are configurable here. Unknown/new events, customer
// events and explicit business/security webhook types bypass this policy.
type notificationKind struct {
	Group      string `json:"group"`
	Paired     bool   `json:"paired"`
	Recovery   bool   `json:"recovery"`
	DefaultOff bool   `json:"defaultOff,omitempty"`
	Cooldown   bool   `json:"cooldown,omitempty"`
}

var notificationKinds = map[string]notificationKind{
	"route_changed":     {Group: "route_changed", DefaultOff: true, Cooldown: true},
	"node_offline":      {Group: "node_availability", Paired: true},
	"node_online":       {Group: "node_availability", Paired: true, Recovery: true},
	"inbound_down":      {Group: "inbound_availability", Paired: true},
	"inbound_up":        {Group: "inbound_availability", Paired: true, Recovery: true},
	"plugin_offline":    {Group: "plugin_availability", Paired: true},
	"plugin_online":     {Group: "plugin_availability", Paired: true, Recovery: true},
	"health":            {Group: "routing_health", Paired: true},
	"apply_failed":      {Group: "apply_failed"},
	"backup_failed":     {Group: "backup_failed"},
	"cert_failed":       {Group: "cert_failed"},
	"node_traffic":      {Group: "node_traffic"},
	"operational_alert": {Group: "operational_alert"},
	"cdn_ips_stale":     {Group: "cdn_ips", Paired: true},
	"cdn_ips_fresh":     {Group: "cdn_ips", Paired: true, Recovery: true},
}

type notificationWindow struct {
	ID       string    `json:"id"`
	NodeIDs  []string  `json:"nodeIds"`
	Channels []string  `json:"channels"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
}

type notificationPolicy struct {
	RepeatWindowSeconds int `json:"repeatWindowSeconds"`
	// Optional operational gates preserve all legacy per-event preferences.
	// They do not affect login/security, customer or transaction notices.
	EnabledChannels map[string]bool            `json:"enabledChannels,omitempty"`
	NotifyRecovery  *bool                      `json:"notifyRecovery,omitempty"`
	Channels        map[string]map[string]bool `json:"channels"`
	Maintenance     []notificationWindow       `json:"maintenance"`
}

func defaultNotificationPolicy() notificationPolicy {
	return notificationPolicy{RepeatWindowSeconds: 300, Channels: map[string]map[string]bool{}, Maintenance: []notificationWindow{}}
}

func validNotificationChannel(channel string) bool {
	for _, c := range notificationChannels {
		if c == channel {
			return true
		}
	}
	return false
}

func (p *notificationPolicy) validate() error {
	if p.RepeatWindowSeconds < 0 || p.RepeatWindowSeconds > 86400 {
		return errors.New("重复告警窗口必须为 0–86400 秒")
	}
	for channel := range p.EnabledChannels {
		if !validNotificationChannel(channel) {
			return fmt.Errorf("未知通知渠道 %q", channel)
		}
	}
	for channel, kinds := range p.Channels {
		if !validNotificationChannel(channel) {
			return fmt.Errorf("未知通知渠道 %q", channel)
		}
		for kind := range kinds {
			if _, ok := notificationKinds[kind]; !ok {
				return fmt.Errorf("不可配置的通知事件 %q", kind)
			}
		}
	}
	if len(p.Maintenance) > 20 {
		return errors.New("最多 20 个维护窗口")
	}
	seen := map[string]bool{}
	for _, w := range p.Maintenance {
		if strings.TrimSpace(w.ID) == "" || len(w.ID) > 64 || seen[w.ID] {
			return errors.New("维护窗口 ID 必须唯一且为 1–64 字节")
		}
		seen[w.ID] = true
		if w.StartsAt.IsZero() || w.EndsAt.IsZero() || !w.EndsAt.After(w.StartsAt) || w.EndsAt.Sub(w.StartsAt) > 7*24*time.Hour {
			return errors.New("维护窗口必须有起止时间且不超过 7 天")
		}
		if len(w.NodeIDs) > 100 || len(w.Channels) > 3 {
			return errors.New("维护窗口最多 100 个节点和 3 个渠道")
		}
		ids := map[string]bool{}
		for _, id := range w.NodeIDs {
			if id == "" || len(id) > 32 || ids[id] {
				return errors.New("维护节点 ID 无效或重复")
			}
			ids[id] = true
		}
		channels := map[string]bool{}
		for _, c := range w.Channels {
			if !validNotificationChannel(c) || channels[c] {
				return errors.New("维护渠道无效或重复")
			}
			channels[c] = true
		}
	}
	if p.Channels == nil {
		p.Channels = map[string]map[string]bool{}
	}
	if p.Maintenance == nil {
		p.Maintenance = []notificationWindow{}
	}
	return nil
}

func (s *Server) loadNotificationPolicy() (notificationPolicy, error) {
	p := defaultNotificationPolicy()
	var row store.Setting
	if err := s.st.DB.First(&row, "key = ?", notificationPolicyKey).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return p, nil
	} else if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(row.Value), &p); err != nil {
		return p, err
	}
	return p, p.validate()
}

func (p notificationPolicy) muted(channel string, e Event, now time.Time) bool {
	if enabled, configured := p.EnabledChannels[channel]; configured && !enabled {
		return true
	}
	if kind, applies := operationalNotificationKind(e); applies && kind.Recovery && p.NotifyRecovery != nil && !*p.NotifyRecovery {
		return true
	}
	enabled, exists := p.Channels[channel][e.Kind]
	if (!exists && notificationKinds[e.Kind].DefaultOff) || (exists && !enabled) {
		return true
	}
	contains := func(items []string, v string) bool {
		for _, item := range items {
			if item == v {
				return true
			}
		}
		return false
	}
	for _, w := range p.Maintenance {
		if now.Before(w.StartsAt) || !now.Before(w.EndsAt) {
			continue
		}
		if len(w.Channels) > 0 && !contains(w.Channels, channel) {
			continue
		}
		if len(w.NodeIDs) > 0 && (e.NodeID == "" || !contains(w.NodeIDs, e.NodeID)) {
			continue
		}
		return true
	}
	return false
}

func notificationHash(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// operationalNotificationKind restricts this policy to operational notices.
func operationalNotificationKind(e Event) (notificationKind, bool) {
	kind, applies := notificationKinds[e.Kind]
	if !applies || e.UserID != "" || e.UserOnly || (e.Type != "" && e.Type != kindTypes[e.Kind]) {
		return kind, false
	}
	if e.Kind == "operational_alert" && (strings.HasPrefix(e.ResourceID, "payment-") || strings.HasPrefix(e.ResourceID, "refund-")) {
		return kind, false
	}
	if e.Kind == "health" {
		down, ok := e.Data["down"].(bool)
		if !ok {
			return kind, false
		}
		kind.Recovery = !down
	}
	return kind, true
}

// operationalNotice reserves an attempt per recipient, not external delivery.
// A database failure fails open instead of silently losing a fault report.
func (s *Server) operationalNotice(channel string, e Event, now time.Time) (Event, bool) {
	kind, applies := operationalNotificationKind(e)
	if !applies || !validNotificationChannel(channel) {
		return e, true
	}
	p, err := s.loadNotificationPolicy()
	if err != nil {
		s.log.Warn("notification policy unavailable", "error", err)
		return e, !kind.DefaultOff
	}
	resource := e.ResourceID
	if resource == "" {
		resource = e.NodeID
	}
	if resource == "" && e.Kind != "backup_failed" {
		resource = notificationHash(e.Text)
	}
	key := notificationHash(channel, e.noticeRecipient, kind.Group, e.NodeID, resource)
	fingerprint := notificationHash(e.Kind, e.Text)
	muted := e.noticeDisabled || p.muted(channel, e, now)
	allow := false
	var merged int64
	token := store.NewID()
	previousOpen := false
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		state := store.NotificationState{Key: key, NodeID: e.NodeID, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&state).Error; err != nil {
			return err
		}
		if err := tx.Model(&store.NotificationState{}).Where("key = ?", key).UpdateColumn("key", gorm.Expr("key")).Error; err != nil {
			return err
		}
		if err := tx.First(&state, "key = ?", key).Error; err != nil {
			return err
		}
		previousOpen = state.Open
		if kind.Paired {
			if kind.Recovery {
				allow = state.Open && !muted
				state.Open = false // a muted recovery must still close the incident
				if !allow {
					state.Suppressed = 0
					if muted {
						state.Token = ""
					}
				}
			} else {
				allow = !state.Open && !muted
				if allow {
					state.Open = true
				}
			}
		} else {
			allow = !muted && (state.LastAttempt == nil || (!kind.Cooldown && state.Fingerprint != fingerprint) || p.RepeatWindowSeconds == 0 || now.Sub(*state.LastAttempt) >= time.Duration(p.RepeatWindowSeconds)*time.Second)
		}
		state.UpdatedAt = now
		if allow {
			state.Token = token
			merged = state.Suppressed
			state.Suppressed = 0
			state.LastAttempt = &now
			state.Fingerprint = fingerprint
		} else if !kind.Recovery {
			state.Suppressed++
		}
		return tx.Save(&state).Error
	})
	if err != nil {
		s.log.Warn("notification state unavailable", "error", err)
		return e, !muted
	}
	if allow {
		e.noticeKey, e.noticeToken, e.noticeMerged, e.noticePreviousOpen = key, token, merged, previousOpen
	}
	if allow && merged > 0 {
		if e.Text != "" {
			e.Text += fmt.Sprintf("\n已合并同类通知 %d 条。", merged)
		}
		data := make(map[string]any, len(e.Data)+1)
		for k, v := range e.Data {
			data[k] = v
		}
		data["notification_suppressed"] = merged
		e.Data = data
	}
	return e, allow
}

// Existing durable webhook deliveries honor silence at retry time too. They
// retain their payload/id and do not consume a second dedup reservation.
func (s *Server) operationalWebhookMuted(d *store.WebhookDelivery, now time.Time) bool {
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal([]byte(d.Payload), &payload) != nil {
		return false
	}
	e := Event{Type: d.Type, Data: payload.Data}
	for kind, typ := range kindTypes {
		if typ == d.Type {
			e.Kind = kind
			break
		}
	}
	e.NodeID, _ = payload.Data["node_id"].(string)
	e.UserID, _ = payload.Data["user_id"].(string)
	if e.Kind == "operational_alert" {
		e.ResourceID, _ = payload.Data["key"].(string)
	}
	kind, applies := operationalNotificationKind(e)
	if !applies {
		return false
	}
	p, err := s.loadNotificationPolicy()
	if err != nil {
		s.log.Warn("webhook notification policy unavailable", "error", err)
		return kind.DefaultOff
	}
	return p.muted("webhook", e, now)
}

// A known failure before durable delivery releases only this recipient's
// reservation. Matching the token prevents an old failure undoing newer state.
func (s *Server) retryOperationalNotice(e Event) {
	if e.noticeKey == "" {
		return
	}
	err := s.st.DB.Model(&store.NotificationState{}).Where("key = ? AND token = ?", e.noticeKey, e.noticeToken).
		Updates(map[string]any{"open": e.noticePreviousOpen, "last_attempt": nil, "token": "", "suppressed": gorm.Expr("suppressed + ?", e.noticeMerged)}).Error
	if err != nil {
		s.log.Warn("notification reservation release failed", "error", err)
	}
}

func (s *Server) getNotificationPolicy(c *gin.Context) {
	p, err := s.loadNotificationPolicy()
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"policy": p, "channels": notificationChannels, "events": notificationKinds, "checkedAt": time.Now().UTC()})
}

func (s *Server) putNotificationPolicy(c *gin.Context) {
	p := defaultNotificationPolicy()
	req := &p
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req == nil {
		badRequest(c, errors.New("配置必须是 JSON 对象"))
		return
	}
	p = *req
	if err := dec.Decode(new(any)); err != io.EOF {
		badRequest(c, errors.New("只接受一个配置对象"))
		return
	}
	if err := p.validate(); err != nil {
		badRequest(c, err)
		return
	}
	// Check node references and replace the complete policy atomically.
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		for _, w := range p.Maintenance {
			if len(w.NodeIDs) == 0 {
				continue
			}
			var count int64
			if err := tx.Model(&store.Node{}).Where("id IN ?", w.NodeIDs).Count(&count).Error; err != nil {
				return err
			}
			if count != int64(len(w.NodeIDs)) {
				return errNotificationNodes
			}
		}
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&store.Setting{Key: notificationPolicyKey, Value: string(b)}).Error
	})
	if err != nil {
		if errors.Is(err, errNotificationNodes) {
			badRequest(c, err)
		} else {
			serverError(c, err)
		}
		return
	}
	s.auditCtx(c, "notification.policy.update", "operational", "")
	s.getNotificationPolicy(c)
}
