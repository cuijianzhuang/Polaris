package panel

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) cdnRegionOptions() []cdnkit.RegionOption {
	out := slices.Clone(cdnkit.Regions)
	var values []string
	s.st.DB.Model(&store.CDNIPReport{}).Distinct("region").Where("region <> ''").Order("region").Pluck("region", &values)
	for _, v := range values {
		code, err := cdnkit.NormalizeRegion(v)
		if err != nil || slices.ContainsFunc(out, func(o cdnkit.RegionOption) bool { return o.Code == code }) {
			continue
		}
		out = append(out, cdnkit.RegionOption{Code: code, Name: code, EN: code})
	}
	return out
}
func (s *Server) listCDNRegions(c *gin.Context) { c.JSON(http.StatusOK, s.cdnRegionOptions()) }
func (s *Server) regionOfIP(ip string) string {
	p, err := readRouteGeoPolicy(s.st.DB)
	if err != nil || !p.Enabled {
		return ""
	}
	db, _ := s.loadRouteGeo(p.SHA256)
	if db == nil {
		return ""
	}
	m := db.Lookup(ip)
	if m.State != "matched" {
		return ""
	}
	if m.CountryCode == "CN" {
		for _, name := range []string{m.Province, m.ProvinceEN} {
			if code, e := cdnkit.NormalizeRegion(name); e == nil && strings.HasPrefix(code, "CN-") {
				return code
			}
		}
	}
	return m.CountryCode
}
func (s *Server) subRegion(r *http.Request, u *store.User) string {
	if v := r.URL.Query().Get("region"); v != "" {
		if v == "all" {
			return ""
		}
		if v != "auto" {
			code, _ := cdnkit.NormalizeRegion(v)
			return code
		}
	} else if u != nil && u.CDNRegion != "" {
		return u.CDNRegion
	}
	return s.regionOfIP(clientIP(r))
}

func (s *Server) cdnAudienceIPs(p *store.CDNProfile, port int, carrier, region string) []string {
	rs, _, _ := cdnkit.AudienceReports(dbStore{s.st.DB}, p, port, carrier, region)
	if len(rs) == 0 {
		return nil
	}
	return cdnkit.UsableReportIPs(p, rs, port)
}

// Keep metadata updates narrow; old clients may still use /u/carrier.
func (s *Server) portalRegion(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(401, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		Region *string `json:"region"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Region == nil {
		badRequest(c, fmt.Errorf("请提供地区，留空表示自动识别"))
		return
	}
	region, err := cdnkit.NormalizeRegion(*req.Region)
	if err != nil {
		badRequest(c, err)
		return
	}
	if err = s.st.DB.Model(u).Update("cdn_region", region).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
