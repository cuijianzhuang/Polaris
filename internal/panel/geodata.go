package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Xray geodata auto-update: settings geodata_cron (empty = off),
// geodata_outbound and geodata_assets (JSON [{url, file}]).

var geodataSettings = map[string]bool{"geodata_cron": true, "geodata_outbound": true, "geodata_assets": true}

// geodataSpec is the block every node's spec carries (nil when off).
func (s *Server) geodataSpec() *model.GeodataSpec {
	cronExpr := strings.TrimSpace(s.st.GetSetting("geodata_cron", ""))
	if cronExpr == "" {
		return nil
	}
	assets := model.StandardGeoAssets
	if raw := s.st.GetSetting("geodata_assets", ""); raw != "" {
		var as []model.GeoAsset
		if json.Unmarshal([]byte(raw), &as) == nil && len(as) > 0 {
			assets = as
		}
	}
	return &model.GeodataSpec{Cron: cronExpr, Outbound: s.st.GetSetting("geodata_outbound", ""), Assets: assets}
}

// validateGeodataSetting checks a geodata_* value before it is stored.
func validateGeodataSetting(k, v string) error {
	switch k {
	case "geodata_cron":
		if strings.TrimSpace(v) == "" {
			return nil
		}
		if _, err := cron.ParseStandard(strings.TrimSpace(v)); err != nil {
			return fmt.Errorf("更新计划不是有效的 cron 表达式（5 段，例如 0 4 * * *）：%w", err)
		}
	case "geodata_assets":
		if v == "" {
			return nil
		}
		var as []model.GeoAsset
		if err := json.Unmarshal([]byte(v), &as); err != nil {
			return fmt.Errorf("geodata_assets: %w", err)
		}
		seen := map[string]bool{}
		for _, a := range as {
			if err := validGeoAsset(a); err != nil {
				return err
			}
			if seen[a.File] {
				return fmt.Errorf("文件名重复：%s", a.File)
			}
			seen[a.File] = true
		}
	}
	return nil
}

func validGeoAsset(a model.GeoAsset) error {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("下载地址必须是 https：%s", a.URL)
	}
	if a.File == "" || a.File != filepath.Base(a.File) || !strings.HasSuffix(a.File, ".dat") {
		return fmt.Errorf("文件名应为不含路径的 .dat 文件：%s", a.File)
	}
	return nil
}

// geodataUpdate is POST /nodes/:id/geodata-update {file}: the agent
// downloads the files now (all when file is empty) and reloads Xray.
func (s *Server) geodataUpdate(c *gin.Context) {
	var req struct {
		File string `json:"file"`
	}
	_ = c.ShouldBindJSON(&req)
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	g := s.geodataSpec()
	assets := model.StandardGeoAssets
	if g != nil {
		assets = g.Assets
	}
	var pick []model.GeoAsset
	for _, a := range assets {
		if req.File == "" || a.File == req.File {
			pick = append(pick, a)
		}
	}
	if len(pick) == 0 {
		badRequest(c, fmt.Errorf("未配置文件 %s", req.File))
		return
	}
	b, _ := json.Marshal(pick)
	res, err := s.hub.Command(c, n.ID, "geodata_update", map[string]string{"assets": string(b)}, 3*time.Minute)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if !res.Ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": res.Error})
		return
	}
	s.auditCtx(c, "node.geodata_update", n.Name, req.File)
	c.JSON(http.StatusOK, gin.H{"ok": true, "output": res.Output})
}
