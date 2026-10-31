package panel

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

type cdnRegionOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
	EN   string `json:"en"`
}

var cdnRegions = []cdnRegionOption{
	{"CN-11", "北京", "Beijing"}, {"CN-12", "天津", "Tianjin"}, {"CN-13", "河北", "Hebei"}, {"CN-14", "山西", "Shanxi"}, {"CN-15", "内蒙古", "Inner Mongolia"},
	{"CN-21", "辽宁", "Liaoning"}, {"CN-22", "吉林", "Jilin"}, {"CN-23", "黑龙江", "Heilongjiang"}, {"CN-31", "上海", "Shanghai"}, {"CN-32", "江苏", "Jiangsu"}, {"CN-33", "浙江", "Zhejiang"}, {"CN-34", "安徽", "Anhui"}, {"CN-35", "福建", "Fujian"}, {"CN-36", "江西", "Jiangxi"}, {"CN-37", "山东", "Shandong"},
	{"CN-41", "河南", "Henan"}, {"CN-42", "湖北", "Hubei"}, {"CN-43", "湖南", "Hunan"}, {"CN-44", "广东", "Guangdong"}, {"CN-45", "广西", "Guangxi"}, {"CN-46", "海南", "Hainan"},
	{"CN-50", "重庆", "Chongqing"}, {"CN-51", "四川", "Sichuan"}, {"CN-52", "贵州", "Guizhou"}, {"CN-53", "云南", "Yunnan"}, {"CN-54", "西藏", "Tibet"}, {"CN-61", "陕西", "Shaanxi"}, {"CN-62", "甘肃", "Gansu"}, {"CN-63", "青海", "Qinghai"}, {"CN-64", "宁夏", "Ningxia"}, {"CN-65", "新疆", "Xinjiang"},
	{"HK", "香港", "Hong Kong"}, {"MO", "澳门", "Macao"}, {"TW", "台湾", "Taiwan"},
}

// Store stable codes, while accepting familiar province names in reports.
func normalizeCDNRegion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	for _, o := range cdnRegions {
		if strings.EqualFold(value, o.Code) || strings.EqualFold(value, o.EN) || value == o.Name || value == o.Name+"省" || value == o.Name+"市" || value == o.Name+"自治区" || value == o.Name+"壮族自治区" || value == o.Name+"回族自治区" || value == o.Name+"维吾尔自治区" || value == o.Name+"特别行政区" {
			return o.Code, nil
		}
	}
	country := strings.ToUpper(value)
	if len(country) == 2 && country[0] >= 'A' && country[0] <= 'Z' && country[1] >= 'A' && country[1] <= 'Z' {
		return country, nil
	}
	return "", fmt.Errorf("地区请使用省份名称、CN-省份代码或两位国家/地区代码")
}
func cdnRegionName(code string) string {
	for _, o := range cdnRegions {
		if o.Code == code {
			return o.Name
		}
	}
	return code
}
func regionFromSource(source string) string {
	source = strings.TrimSpace(source)
	for _, o := range cdnRegions {
		if strings.HasPrefix(source, o.Name) || strings.HasPrefix(strings.ToLower(source), strings.ToLower(o.EN)+" ") || strings.HasPrefix(strings.ToUpper(source), o.Code+"-") {
			return o.Code
		}
	}
	return ""
}
func effectiveCDNRegion(r store.CDNIPReport) string {
	if r.Region != "" {
		code, _ := normalizeCDNRegion(r.Region)
		return code
	}
	return regionFromSource(r.Source)
}
func (s *Server) cdnRegionOptions() []cdnRegionOption {
	out := slices.Clone(cdnRegions)
	var values []string
	s.st.DB.Model(&store.CDNIPReport{}).Distinct("region").Where("region <> ''").Order("region").Pluck("region", &values)
	for _, v := range values {
		code, err := normalizeCDNRegion(v)
		if err != nil || slices.ContainsFunc(out, func(o cdnRegionOption) bool { return o.Code == code }) {
			continue
		}
		out = append(out, cdnRegionOption{code, code, code})
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
			if code, e := normalizeCDNRegion(name); e == nil && strings.HasPrefix(code, "CN-") {
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
			code, _ := normalizeCDNRegion(v)
			return code
		}
	} else if u != nil && u.CDNRegion != "" {
		return u.CDNRegion
	}
	return s.regionOfIP(clientIP(r))
}

type cdnAudience struct {
	Region   string
	Personal map[string][]string
}

func audienceRegion(a []cdnAudience) string {
	if len(a) > 0 {
		return a[0].Region
	}
	return ""
}

// Reports are isolated by the actual protocol and port before audience ranking.
// Known carrier: exact province+carrier, then that carrier nationwide, then merged.
// Unknown carrier: the province's reports, then merged. Old unlabeled reports
// still participate in the merged pool, or infer province from their source name.
func (s *Server) cdnAudienceReports(p *store.CDNProfile, port int, carrier, region string) ([]store.CDNIPReport, string) {
	if p.RegionSplit && region != "" {
		filterCarrier := ""
		if p.CarrierSplit {
			filterCarrier = carrier
		}
		reports := cdnReportsForPort(s.st.DB, p, port, cdnSelectionCutoff(p), filterCarrier, region)
		if len(reports) > 0 {
			if filterCarrier != "" {
				return reports, "region_carrier"
			}
			return reports, "region"
		}
	}
	if p.CarrierSplit && carrier != "" {
		reports := cdnReportsForPort(s.st.DB, p, port, cdnSelectionCutoff(p), carrier)
		if len(reports) > 0 {
			return reports, "carrier"
		}
	}
	return nil, "merged"
}

func (s *Server) cdnAudienceIPs(p *store.CDNProfile, port int, carrier, region string) []string {
	rs, _ := s.cdnAudienceReports(p, port, carrier, region)
	if len(rs) == 0 {
		return nil
	}
	return usableCDNIPsAt(p, withPinned(mergeCDNReports(rs), p.PinnedHosts.V), port)
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
	region, err := normalizeCDNRegion(*req.Region)
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

// Canonicalize only the report being written. Legacy rows remain compatible on read.
func reportRegion(req cdnIPReportReq) (string, error) {
	if req.Region != "" {
		return normalizeCDNRegion(req.Region)
	}
	return regionFromSource(req.Source), nil
}

// Snapshot resolved choices for converter callbacks: its network must not replace
// the subscriber's region/carrier. "all" explicitly disables auto detection.
func withCDNAudience(raw, carrier, region string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("carrier", orDefault(carrier, "all"))
	q.Set("region", orDefault(region, "all"))
	u.RawQuery = q.Encode()
	return u.String()
}
func cdnExplicitSelectionURL(raw string, r *http.Request) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for _, key := range []string{"carrier", "region"} {
		v := r.URL.Query().Get(key)
		if v == "" {
			continue
		}
		if key == "region" && v != "auto" && v != "all" {
			var e error
			v, e = normalizeCDNRegion(v)
			if e != nil {
				continue
			}
		}
		if key == "carrier" && v != "auto" && v != "all" {
			if _, ok := cdnCarriers[v]; !ok {
				continue
			}
		}
		q.Set(key, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
