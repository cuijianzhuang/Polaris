package cdnkit

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// RegionOption is one region a subscriber may pick for preferred IPs.
type RegionOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
	EN   string `json:"en"`
}

// Regions are the provinces, municipalities and special regions, in
// GB/T 2260 order, plus Hong Kong, Macao and Taiwan.
var Regions = []RegionOption{
	{"CN-11", "北京", "Beijing"}, {"CN-12", "天津", "Tianjin"}, {"CN-13", "河北", "Hebei"}, {"CN-14", "山西", "Shanxi"}, {"CN-15", "内蒙古", "Inner Mongolia"},
	{"CN-21", "辽宁", "Liaoning"}, {"CN-22", "吉林", "Jilin"}, {"CN-23", "黑龙江", "Heilongjiang"}, {"CN-31", "上海", "Shanghai"}, {"CN-32", "江苏", "Jiangsu"}, {"CN-33", "浙江", "Zhejiang"}, {"CN-34", "安徽", "Anhui"}, {"CN-35", "福建", "Fujian"}, {"CN-36", "江西", "Jiangxi"}, {"CN-37", "山东", "Shandong"},
	{"CN-41", "河南", "Henan"}, {"CN-42", "湖北", "Hubei"}, {"CN-43", "湖南", "Hunan"}, {"CN-44", "广东", "Guangdong"}, {"CN-45", "广西", "Guangxi"}, {"CN-46", "海南", "Hainan"},
	{"CN-50", "重庆", "Chongqing"}, {"CN-51", "四川", "Sichuan"}, {"CN-52", "贵州", "Guizhou"}, {"CN-53", "云南", "Yunnan"}, {"CN-54", "西藏", "Tibet"}, {"CN-61", "陕西", "Shaanxi"}, {"CN-62", "甘肃", "Gansu"}, {"CN-63", "青海", "Qinghai"}, {"CN-64", "宁夏", "Ningxia"}, {"CN-65", "新疆", "Xinjiang"},
	{"HK", "香港", "Hong Kong"}, {"MO", "澳门", "Macao"}, {"TW", "台湾", "Taiwan"},
}

// NormalizeRegion stores stable codes, while accepting familiar province
// names in reports.
func NormalizeRegion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	for _, o := range Regions {
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

// RegionName is the Chinese name of a region code (the code itself when unknown).
func RegionName(code string) string {
	for _, o := range Regions {
		if o.Code == code {
			return o.Name
		}
	}
	return code
}

// RegionFromSource infers a region code from a speed-test point's name.
func RegionFromSource(source string) string {
	source = strings.TrimSpace(source)
	for _, o := range Regions {
		if strings.HasPrefix(source, o.Name) || strings.HasPrefix(strings.ToLower(source), strings.ToLower(o.EN)+" ") || strings.HasPrefix(strings.ToUpper(source), o.Code+"-") {
			return o.Code
		}
	}
	return ""
}

// EffectiveRegion is the region a report counts for: the one it names, or
// the one its source name implies.
func EffectiveRegion(r store.CDNIPReport) string {
	if r.Region != "" {
		code, _ := NormalizeRegion(r.Region)
		return code
	}
	return RegionFromSource(r.Source)
}

// ReportRegion canonicalises only the report being written. Legacy rows
// remain compatible on read.
func ReportRegion(region, source string) (string, error) {
	if region != "" {
		return NormalizeRegion(region)
	}
	return RegionFromSource(source), nil
}

// Audience is the subscriber-side selection a subscription renders for:
// the resolved region plus the user's personal IPs and pools.
type Audience struct {
	Region   string
	Personal map[string][]string
	Pools    map[string]store.PersonalCDNPool
}

// AudienceRegion is the region of the first audience ("" when none).
func AudienceRegion(a []Audience) string {
	if len(a) > 0 {
		return a[0].Region
	}
	return ""
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// WithAudience snapshots resolved choices for converter callbacks: its
// network must not replace the subscriber's region/carrier. "all"
// explicitly disables auto detection.
func WithAudience(raw, carrier, region string) string {
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

// ExplicitSelectionURL carries an explicit carrier/region choice from a
// subscription request into the link it renders. q is the request's query.
func ExplicitSelectionURL(raw string, q url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := u.Query()
	for _, key := range []string{"carrier", "region"} {
		v := q.Get(key)
		if v == "" {
			continue
		}
		if key == "region" && v != "auto" && v != "all" {
			var e error
			v, e = NormalizeRegion(v)
			if e != nil {
				continue
			}
		}
		if key == "carrier" && v != "auto" && v != "all" {
			if !ValidCarrier(v) {
				continue
			}
		}
		query.Set(key, v)
	}
	u.RawQuery = query.Encode()
	return u.String()
}
