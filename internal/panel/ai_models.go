package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Fetch from the configured provider, never from an address supplied by the
// model or a portal user. This also supports discovery before first save.
func (s *Server) aiModels(c *gin.Context) {
	if !aiOwner(c) {
		return
	}
	var req struct {
		Config   aiConfig `json:"config"`
		ClearKey bool     `json:"clearKey"`
	}
	if !aiReadJSON(c, &req) {
		return
	}
	cfg := req.Config
	cfg.Enabled = false
	if err := validateAIConfig(&cfg); err != nil {
		badRequest(c, err)
		return
	}
	old, err := s.aiConfig()
	if err != nil {
		serverError(c, errors.New("AI 设置读取失败"))
		return
	}
	if (cfg.APIKey != "" || cfg.BaseURL != old.BaseURL || cfg.Provider != old.Provider || cfg.Format != old.Format || cfg.AllowPrivate != old.AllowPrivate) && !s.sudoOK(c) {
		sudoRequired(c)
		return
	}
	key := strings.TrimSpace(cfg.APIKey)
	if len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		badRequest(c, errors.New("API Key 格式无效"))
		return
	}
	if key == "" && !req.ClearKey && old.APIKey != "" {
		if old.BaseURL != cfg.BaseURL || old.Provider != cfg.Provider || old.Format != cfg.Format {
			badRequest(c, errors.New("请为新服务填写 API Key"))
			return
		}
		key = s.st.Box.MustOpen(old.APIKey)
	}
	if cfg.Provider != "custom" && key == "" {
		badRequest(c, errors.New("请先填写 API Key"))
		return
	}
	if !s.countWindow("ai-models:"+c.GetString("adminID"), 5, time.Minute) {
		c.Header("Retry-After", "60")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "模型列表请求过多，请稍后重试"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	models := []aiModel{}
	next := ""
	seen := map[string]bool{}
	for page := 0; page < 4; page++ {
		endpoint := cfg.BaseURL + "/models"
		if next != "" {
			endpoint += "?after_id=" + next
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			badRequest(c, errors.New("API 地址无效"))
			return
		}
		if cfg.Format == "anthropic" {
			r.Header.Set("x-api-key", key)
			r.Header.Set("anthropic-version", "2023-06-01")
		} else if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		client := safeClientWithPrivatePolicy(20*time.Second, cfg.AllowPrivate)
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		response, err := client.Do(r)
		if err != nil {
			client.CloseIdleConnections()
			c.JSON(http.StatusBadGateway, gin.H{"error": "模型列表获取失败，请检查地址和网络；也可手动填写模型名称"})
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (512<<10)+1))
		response.Body.Close()
		client.CloseIdleConnections()
		if response.StatusCode != 200 || readErr != nil || len(body) > 512<<10 {
			c.JSON(http.StatusBadGateway, gin.H{"error": "服务商未返回可用模型列表，请检查密钥和接口，或手动填写模型名称"})
			return
		}
		var result struct {
			Data    []aiModel `json:"data"`
			HasMore bool      `json:"has_more"`
			LastID  string    `json:"last_id"`
		}
		if json.Unmarshal(body, &result) != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "模型列表格式无效，可手动填写模型名称"})
			return
		}
		for _, m := range result.Data {
			if m.ID == "" || len(m.ID) > 160 || strings.ContainsAny(m.ID, "\r\n") || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			models = append(models, m)
		}
		if !result.HasMore || result.LastID == "" {
			break
		}
		// Provider-issued IDs are escaped, never treated as an URL.
		next = aiModelCursor(result.LastID)
	}
	sort.SliceStable(models, func(i, j int) bool {
		if left, right := aiModelPublished(models[i]), aiModelPublished(models[j]); left != right {
			return left > right
		}
		return models[i].ID < models[j].ID
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"models": models, "checkedAt": time.Now().UTC(), "note": "模型列表由服务商实时返回，按发布时间排序（如有）；具体调用权限和接口兼容性请用连接测试确认"})
}

type aiModel struct {
	ID        string `json:"id"`
	Name      string `json:"display_name,omitempty"`
	Created   int64  `json:"created,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

func aiModelCursor(id string) string { return url.QueryEscape(id) }

func aiModelPublished(m aiModel) int64 {
	if m.Created > 0 {
		return m.Created
	}
	if when, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		return when.Unix()
	}
	return 0
}
