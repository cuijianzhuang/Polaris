package panel

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Preferred IPs by carrier.
//
// A CDN IP that is fast on China Telecom may be slow on China Mobile, so a
// profile with CarrierSplit hands each subscriber the IPs that speed-test
// points on the subscriber's own carrier reported. The carrier comes from,
// in order: ?carrier= on the subscription link, the user's choice in the
// user center, then the request's source IP looked up in the route ASN
// database (Settings → network checks). When none of these is known, or no
// fresh report from that carrier exists, the profile's merged list is used.

// Carriers: China Telecom, China Unicom, China Mobile.
var cdnCarriers = map[string]string{"ct": "电信", "cu": "联通", "cm": "移动"}

// carrierASNs maps the main mainland backbone and metro ASNs to a carrier.
var carrierASNs = map[uint32]string{
	4134: "ct", 4809: "ct", 4812: "ct", 4811: "ct", 4813: "ct", 4816: "ct", 23724: "ct", 134774: "ct", 136188: "ct", 140061: "ct", 58466: "ct", 137693: "ct", 134420: "ct",
	4837: "cu", 9929: "cu", 17621: "cu", 17622: "cu", 17623: "cu", 17816: "cu", 4808: "cu", 9800: "cu", 136958: "cu", 135061: "cu", 133119: "cu",
	9808: "cm", 56040: "cm", 56041: "cm", 56042: "cm", 56044: "cm", 56046: "cm", 56047: "cm", 56048: "cm", 24400: "cm", 24444: "cm", 24445: "cm", 24547: "cm", 38019: "cm", 132525: "cm", 134810: "cm",
}

// carrierFromName reads a carrier from a speed-test point's name, e.g.
// "上海电信" or "Beijing Unicom".
func carrierFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "电信") || strings.Contains(n, "telecom") || strings.Contains(n, "chinanet"):
		return "ct"
	case strings.Contains(n, "联通") || strings.Contains(n, "unicom"):
		return "cu"
	case strings.Contains(n, "移动") || strings.Contains(n, "mobile") || strings.Contains(n, "cmcc"):
		return "cm"
	}
	return ""
}

// reportCarrier is the carrier a report counts for: the one it names, or
// the one its source name implies.
func reportCarrier(req cdnIPReportReq) (string, error) {
	c := strings.ToLower(strings.TrimSpace(req.Carrier))
	if c == "" {
		return carrierFromName(req.Source), nil
	}
	if _, ok := cdnCarriers[c]; !ok {
		return "", errors.New("carrier 只能是 ct（电信）、cu（联通）、cm（移动）或留空")
	}
	return c, nil
}

// subCarrier resolves the carrier for a subscription request.
func (s *Server) subCarrier(r *http.Request, u *store.User) string {
	if c := strings.ToLower(r.URL.Query().Get("carrier")); c != "" {
		if _, ok := cdnCarriers[c]; ok {
			return c
		}
		if c != "auto" {
			return ""
		}
	} else if _, ok := cdnCarriers[u.CDNCarrier]; ok {
		return u.CDNCarrier
	}
	return s.carrierOfIP(clientIP(r))
}

// carrierOfIP looks the address up in the route ASN database, when one is
// enabled; "" when it is not, or the network is not a known carrier.
func (s *Server) carrierOfIP(ip string) string {
	p, err := readRouteASNPolicy(s.st.DB)
	if err != nil || !p.Enabled {
		return ""
	}
	db, _ := s.loadRouteASN(p.SHA256)
	if db == nil {
		return ""
	}
	if m := db.Lookup(ip); m.State == "matched" {
		return carrierASNs[m.ASN]
	}
	return ""
}

// carrierCDNIPs is the profile's list for one carrier: the fresh reports of
// that carrier's speed-test points, merged and filtered like the main list.
// nil when there is none, and the caller falls back to the main list.
func (s *Server) carrierCDNIPs(p *store.CDNProfile, carrier string, ports ...int) []string {
	port := cdnDefaultPort(p)
	if len(ports) > 0 {
		port = ports[0]
	}
	if !p.CarrierSplit || carrier == "" {
		return nil
	}
	reports := cdnReportsForPort(s.st.DB, p, port, time.Now().Add(-cdnStaleAfter(p)), carrier)
	if len(reports) == 0 {
		return nil
	}
	list := withPinned(mergeCDNReports(reports), p.PinnedHosts.V)

	return usableCDNIPsAt(p, list, port)
}

// portalCarrier is POST /u/carrier {carrier}: the user picks the carrier
// whose preferred IPs their subscription uses ("" = detect).
func (s *Server) portalCarrier(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		Carrier string `json:"carrier"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if _, ok := cdnCarriers[req.Carrier]; !ok && req.Carrier != "" {
		badRequest(c, errors.New("请选择电信、联通、移动或自动"))
		return
	}
	if err := s.st.DB.Model(u).Update("cdn_carrier", req.Carrier).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// cdnCarrierSplitOn reports whether any profile hands out IPs by carrier;
// the user center only offers the choice then.
func (s *Server) cdnCarrierSplitOn() bool {
	var n int64
	s.st.DB.Model(&store.CDNProfile{}).Where("carrier_split = ?", true).Count(&n)
	return n > 0
}
