package panel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Webhooks deliver events to outside services: plugins (signed JSON) or
// chat tools (Discord, Slack, WeCom, DingTalk, Feishu) with a text message.
// Every delivery is stored and retried with backoff until it succeeds.

// kindTypes names the internal event kinds for the outside world.
var kindTypes = map[string]string{
	"node_offline": "node.offline", "node_online": "node.online", "apply_failed": "node.apply_failed",
	"node_traffic": "node.traffic_alert", "server_renewal": "node.renewal_due", "health": "routing.health",
	"inbound_down": "inbound.down", "inbound_up": "inbound.up",
	"user_limited": "user.traffic_exceeded", "user_expired": "user.expired", "user_active": "user.reactivated",
	"user_renewed": "user.renewed", "ip_limit": "user.ip_limit", "traffic_warn": "user.traffic_warn",
	"expire_warn": "user.expire_warn", "traffic_spike": "user.traffic_spike",
	"login_ban": "admin.login_banned", "admin_login_new_ip": "admin.login_new_ip", "admin_recovery_code": "admin.recovery_code_used",
	"backup_failed": "system.backup_failed", "cert_failed": "system.cert_failed", "license": "system.license",
	"panel_update": "system.panel_update", "plugin_offline": "system.plugin_offline", "plugin_online": "system.plugin_online",
}

// EventTypes lists every type a webhook can subscribe to.
var EventTypes = []string{
	"user.created", "user.updated", "user.deleted", "user.renewed", "user.traffic_added", "user.traffic_reset", "user.token_reset",
	"user.bulk", "user.expired", "user.traffic_exceeded", "user.reactivated", "user.traffic_warn", "user.expire_warn",
	"user.ip_limit", "user.traffic_spike", "user.plan_changed", "order.paid", "order.failed",
	"node.offline", "node.online", "node.apply_failed", "node.traffic_alert", "node.renewal_due",
	"inbound.down", "inbound.up", "routing.health",
	"admin.login_banned", "admin.login_new_ip", "admin.recovery_code_used",
	"system.backup_failed", "system.cert_failed", "system.license", "system.panel_update",
	"system.plugin_offline", "system.plugin_online",
}

// eventType is the dotted type of an event ("user.expired").
func eventType(e Event) string {
	switch {
	case e.Type != "":
		return e.Type
	case strings.Contains(e.Kind, "."):
		return e.Kind
	}
	return kindTypes[e.Kind]
}

var webhookFormats = []string{"json", "discord", "slack", "wecom", "dingtalk", "feishu"}

// webhookRetry is the wait before attempt n+1 (attempt 1 is immediate).
var webhookRetry = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}

// matchEvent: an empty list subscribes to everything; "user.*" to a prefix.
func matchEvent(subs []string, typ string) bool {
	if len(subs) == 0 {
		return true
	}
	for _, s := range subs {
		if s == typ || s == "*" || (strings.HasSuffix(s, ".*") && strings.HasPrefix(typ, strings.TrimSuffix(s, "*"))) {
			return true
		}
	}
	return false
}

// userBrief is the user as events describe it.
func userBrief(u *store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "status": u.Status, "expire_at": u.ExpireAt,
		"traffic_limit": u.TrafficLimit, "used_up": u.UsedUp, "used_down": u.UsedDown, "telegram_id": u.TelegramID, "remark": u.Remark, "plan_id": u.PlanID}
}

// userEvent notifies a user change. Its Text is empty: these are for
// webhooks, not Telegram.
func (s *Server) userEvent(typ string, u *store.User, extra map[string]any) {
	data := map[string]any{"user": userBrief(u)}
	for k, v := range extra {
		data[k] = v
	}
	s.notify(Event{Kind: typ, UserID: u.ID, Data: data})
}

// eventPayload builds the JSON body of an event.
func (s *Server) eventPayload(e Event, typ string) map[string]any {
	data := map[string]any{}
	if e.Text != "" {
		data["message"] = e.Text
	}
	if e.NodeID != "" {
		data["node_id"] = e.NodeID
		var n store.Node
		if s.st.DB.Select("id", "name").First(&n, "id = ?", e.NodeID).Error == nil {
			data["node"] = map[string]any{"id": n.ID, "name": n.Name}
		}
	}
	for k, v := range e.Data {
		data[k] = v
	}
	// The user as stored now; a deleted user keeps the snapshot from Data.
	if e.UserID != "" {
		data["user_id"] = e.UserID
		var u store.User
		if s.st.DB.First(&u, "id = ?", e.UserID).Error == nil {
			data["user"] = userBrief(&u)
		}
	}
	return map[string]any{"id": "evt_" + store.NewID(), "type": typ, "created_at": time.Now().UTC().Format(time.RFC3339), "data": data}
}

// runWebhooks turns events into deliveries.
func (s *Server) runWebhooks(ctx context.Context, events <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			typ := eventType(e)
			if typ == "" {
				continue
			}
			var hooks []store.Webhook
			s.st.DB.Where("enabled = ?", true).Find(&hooks)
			var payload map[string]any
			for _, h := range hooks {
				if !matchEvent(h.Events.V, typ) {
					continue
				}
				if payload == nil {
					payload = s.eventPayload(e, typ)
				}
				body, _ := json.Marshal(payload)
				d := store.WebhookDelivery{ID: store.NewID(), WebhookID: h.ID, EventID: payload["id"].(string), Type: typ,
					Payload: string(body), Status: "pending", NextAt: time.Now()}
				if s.st.DB.Create(&d).Error == nil {
					go s.deliverWebhook(d.ID)
				}
			}
		}
	}
}

// retryWebhooks re-sends deliveries whose next attempt is due.
func (s *Server) retryWebhooks() {
	var ds []store.WebhookDelivery
	s.st.DB.Select("id").Where("status = ? AND next_at <= ?", "pending", time.Now()).Order("next_at").Limit(50).Find(&ds)
	for _, d := range ds {
		s.deliverWebhook(d.ID)
	}
}

// webhookSem caps concurrent deliveries.
var webhookSem = make(chan struct{}, 8)

func (s *Server) deliverWebhook(id string) {
	webhookSem <- struct{}{}
	defer func() { <-webhookSem }()
	// Claim the row so a retry sweep and the first attempt don't both send.
	claim := s.st.DB.Model(&store.WebhookDelivery{}).Where("id = ? AND status = ?", id, "pending").Update("status", "sending")
	if claim.RowsAffected == 0 {
		return
	}
	var d store.WebhookDelivery
	s.st.DB.First(&d, "id = ?", id)
	var h store.Webhook
	if s.st.DB.First(&h, "id = ?", d.WebhookID).Error != nil {
		s.st.DB.Model(&d).Updates(map[string]any{"status": "failed", "last_error": "webhook 已删除"})
		return
	}
	code, err := s.sendWebhook(&h, &d)
	now := time.Now()
	upd := map[string]any{"attempts": d.Attempts + 1, "last_code": code, "last_at": now}
	switch {
	case err == nil:
		upd["status"], upd["last_error"], upd["delivered_at"] = "success", "", now
	case d.Attempts < len(webhookRetry):
		upd["status"], upd["last_error"], upd["next_at"] = "pending", truncate(err.Error(), 500), now.Add(webhookRetry[d.Attempts])
	default:
		upd["status"], upd["last_error"] = "failed", truncate(err.Error(), 500)
	}
	s.st.DB.Model(&d).Updates(upd)
	s.st.DB.Model(&store.Webhook{}).Where("id = ?", h.ID).Updates(map[string]any{"last_status": upd["status"], "last_at": now})
}

var webhookClient = &http.Client{Timeout: 10 * time.Second}

// sendWebhook makes one attempt; any 2xx answer is success.
func (s *Server) sendWebhook(h *store.Webhook, d *store.WebhookDelivery) (int, error) {
	secret := ""
	if h.Secret != "" {
		secret, _ = s.st.Box.Open(h.Secret)
	}
	target := h.URL
	body := []byte(d.Payload)
	hdr := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"Polaris-Webhook/" + Version}}
	if h.Format == "" || h.Format == "json" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		hdr.Set("X-Polaris-Event", d.Type)
		hdr.Set("X-Polaris-Delivery", d.ID)
		if secret != "" {
			hdr.Set("X-Polaris-Signature", "t="+ts+",v1="+signWebhook(secret, ts, body))
		}
	} else {
		var p struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		json.Unmarshal(body, &p)
		text := s.eventText(p.Type, p.Data)
		var err error
		if body, target, err = chatBody(h.Format, text, target, secret); err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header = hdr
	resp, err := webhookClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp.StatusCode, nil
}

// signWebhook is the v1 signature: hex HMAC-SHA256 over "<ts>.<body>".
func signWebhook(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// eventText is the one-line message chat formats send.
func (s *Server) eventText(typ string, data map[string]any) string {
	site := s.st.GetSetting("site_title", "Polaris")
	if m, _ := data["message"].(string); m != "" {
		return "[" + site + "] " + m
	}
	who := ""
	if u, ok := data["user"].(map[string]any); ok {
		who, _ = u["username"].(string)
	}
	label := map[string]string{"user.created": "新建用户", "user.updated": "修改用户", "user.deleted": "删除用户",
		"user.traffic_added": "增加流量", "user.traffic_reset": "重置流量", "user.token_reset": "重置订阅链接",
		"user.bulk": "批量操作用户", "webhook.test": "测试消息：Webhook 配置正常"}[typ]
	if label == "" {
		label = typ
	}
	if who != "" {
		label += "：" + who
	}
	return "[" + site + "] " + label
}

// chatBody formats text for a chat tool; DingTalk and Feishu sign with the
// secret when one is set.
func chatBody(format, text, target, secret string) ([]byte, string, error) {
	var v any
	switch format {
	case "discord":
		v = map[string]any{"content": text}
	case "slack":
		v = map[string]any{"text": text}
	case "wecom":
		v = map[string]any{"msgtype": "text", "text": map[string]any{"content": text}}
	case "dingtalk":
		v = map[string]any{"msgtype": "text", "text": map[string]any{"content": text}}
		if secret != "" {
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			m := hmac.New(sha256.New, []byte(secret))
			m.Write([]byte(ts + "\n" + secret))
			u, err := url.Parse(target)
			if err != nil {
				return nil, "", err
			}
			q := u.Query()
			q.Set("timestamp", ts)
			q.Set("sign", base64.StdEncoding.EncodeToString(m.Sum(nil)))
			u.RawQuery = q.Encode()
			target = u.String()
		}
	case "feishu":
		body := map[string]any{"msg_type": "text", "content": map[string]any{"text": text}}
		if secret != "" {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			m := hmac.New(sha256.New, []byte(ts+"\n"+secret))
			body["timestamp"], body["sign"] = ts, base64.StdEncoding.EncodeToString(m.Sum(nil))
		}
		v = body
	default:
		return nil, "", fmt.Errorf("未知格式 %q", format)
	}
	b, err := json.Marshal(v)
	return b, target, err
}

// checkEvents accepts known types, "*" and group patterns ("user.*").
func checkEvents(evs []string) error {
	for _, e := range evs {
		if e != "*" && !slices.Contains(EventTypes, e) && !(strings.HasSuffix(e, ".*") && slices.ContainsFunc(EventTypes, func(t string) bool { return strings.HasPrefix(t, strings.TrimSuffix(e, "*")) })) {
			return fmt.Errorf("未知事件 %q", e)
		}
	}
	return nil
}

// --- API ---

type webhookReq struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Format       string   `json:"format"`
	Events       []string `json:"events"`
	Enabled      *bool    `json:"enabled"`
	Secret       *string  `json:"secret"`       // chat formats: the robot's signing secret; nil = unchanged
	RotateSecret bool     `json:"rotateSecret"` // json format: issue a new signing secret
}

func (r *webhookReq) check() error {
	u, err := url.Parse(strings.TrimSpace(r.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("地址应为 http(s):// 开头的完整网址")
	}
	if r.Format == "" {
		r.Format = "json"
	}
	if !slices.Contains(webhookFormats, r.Format) {
		return fmt.Errorf("未知格式 %q", r.Format)
	}
	if err := checkEvents(r.Events); err != nil {
		return err
	}
	if r.Name = strings.TrimSpace(r.Name); r.Name == "" {
		r.Name = u.Host
	}
	return nil
}

type webhookView struct {
	store.Webhook
	HasSecret bool `json:"hasSecret"`
}

func (s *Server) listWebhooks(c *gin.Context) {
	var hs []store.Webhook
	s.st.DB.Order("created_at").Find(&hs)
	out := make([]webhookView, len(hs))
	for i, h := range hs {
		out[i] = webhookView{h, h.Secret != ""}
	}
	c.JSON(http.StatusOK, gin.H{"webhooks": out, "events": EventTypes, "formats": webhookFormats})
}

func (s *Server) createWebhook(c *gin.Context) {
	var req webhookReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := req.check(); err != nil {
		badRequest(c, err)
		return
	}
	h, secret := s.newWebhook(req, "")
	if err := s.st.DB.Create(h).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "webhook.create", h.Name, h.URL)
	c.JSON(http.StatusCreated, gin.H{"webhook": webhookView{*h, h.Secret != ""}, "secret": secret})
}

// newWebhook builds a webhook from a checked request; for the JSON format it
// issues the signing secret and returns it.
func (s *Server) newWebhook(req webhookReq, pluginID string) (*store.Webhook, string) {
	h := &store.Webhook{ID: store.NewID(), Name: req.Name, URL: strings.TrimSpace(req.URL), Format: req.Format, PluginID: pluginID,
		Events: store.JSON[[]string]{V: req.Events}, Enabled: req.Enabled == nil || *req.Enabled}
	secret := ""
	if req.Format == "json" {
		secret = "whsec_" + pki.Token(32)
	} else if req.Secret != nil {
		secret = strings.TrimSpace(*req.Secret)
	}
	if secret != "" {
		h.Secret = s.st.Box.Seal(secret)
	}
	if req.Format != "json" {
		secret = "" // the admin typed it; don't echo it back
	}
	return h, secret
}

func (s *Server) updateWebhook(c *gin.Context) {
	var h store.Webhook
	if err := s.st.DB.First(&h, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if h.PluginID != "" {
		badRequest(c, errors.New("这个 Webhook 属于插件，请在「插件」页面管理"))
		return
	}
	var req webhookReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := req.check(); err != nil {
		badRequest(c, err)
		return
	}
	upd := map[string]any{"name": req.Name, "url": strings.TrimSpace(req.URL), "format": req.Format, "events": store.JSON[[]string]{V: req.Events}}
	if req.Enabled != nil {
		upd["enabled"] = *req.Enabled
	}
	secret := ""
	switch {
	case req.Format == "json" && (req.RotateSecret || h.Format != "json" || h.Secret == ""):
		secret = "whsec_" + pki.Token(32)
		upd["secret"] = s.st.Box.Seal(secret)
	case req.Format != "json" && req.Secret != nil:
		upd["secret"] = ""
		if v := strings.TrimSpace(*req.Secret); v != "" {
			upd["secret"] = s.st.Box.Seal(v)
		}
	}
	s.st.DB.Model(&h).Updates(upd)
	s.auditCtx(c, "webhook.update", req.Name, req.URL)
	s.st.DB.First(&h, "id = ?", h.ID)
	c.JSON(http.StatusOK, gin.H{"webhook": webhookView{h, h.Secret != ""}, "secret": secret})
}

func (s *Server) deleteWebhook(c *gin.Context) {
	var h store.Webhook
	if err := s.st.DB.First(&h, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if h.PluginID != "" {
		badRequest(c, errors.New("这个 Webhook 属于插件，请在「插件」页面卸载插件"))
		return
	}
	s.st.DB.Where("webhook_id = ?", h.ID).Delete(&store.WebhookDelivery{})
	s.st.DB.Delete(&h)
	s.auditCtx(c, "webhook.delete", h.Name, h.URL)
	c.Status(http.StatusNoContent)
}

// testWebhook sends a webhook.test event to one webhook and waits for it.
func (s *Server) testWebhook(c *gin.Context) {
	var h store.Webhook
	if err := s.st.DB.First(&h, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	payload := s.eventPayload(Event{Type: "webhook.test", Data: map[string]any{"webhook": h.Name}}, "webhook.test")
	body, _ := json.Marshal(payload)
	d := store.WebhookDelivery{ID: store.NewID(), WebhookID: h.ID, EventID: payload["id"].(string), Type: "webhook.test",
		Payload: string(body), Status: "pending", NextAt: time.Now().Add(time.Hour)}
	s.st.DB.Create(&d)
	s.deliverWebhook(d.ID)
	// A test is not retried: the admin sees the result right away.
	s.st.DB.Model(&store.WebhookDelivery{}).Where("id = ? AND status = ?", d.ID, "pending").Update("status", "failed")
	s.st.DB.First(&d, "id = ?", d.ID)
	c.JSON(http.StatusOK, d)
}

func (s *Server) listDeliveries(c *gin.Context) {
	var ds []store.WebhookDelivery
	s.st.DB.Where("webhook_id = ?", c.Param("id")).Order("created_at desc").Limit(50).Find(&ds)
	c.JSON(http.StatusOK, ds)
}

// retryDelivery sends a delivery again now (failed ones included).
func (s *Server) retryDelivery(c *gin.Context) {
	var d store.WebhookDelivery
	if err := s.st.DB.First(&d, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Model(&d).Updates(map[string]any{"status": "pending", "next_at": time.Now()})
	s.deliverWebhook(d.ID)
	s.st.DB.First(&d, "id = ?", d.ID)
	c.JSON(http.StatusOK, d)
}
