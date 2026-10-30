package panel

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

var errCDNProbeHTTP = errors.New("当前一键测速程序仅支持 HTTPS，HTTP 配置档请使用来源 URL 或自行上报")

// Pairing a cf-ip-probe speed-test point: the panel creates a token that can
// only report preferred IPs for one profile under one source name, and the
// admin copies a ready install command. Removing the point revokes it.

type cdnProbeView struct {
	Port       int        `json:"port"`
	Source     string     `json:"source"`
	TokenID    string     `json:"tokenId"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	LastIP     string     `json:"lastIp"`
}

// cdnProbes returns the paired points of the given profiles.
func (s *Server) cdnProbes(ids []string) map[string][]cdnProbeView {
	out := map[string][]cdnProbeView{}
	if len(ids) == 0 {
		return out
	}
	var ts []store.APIToken
	s.st.DB.Where("cdn_profile_id IN ?", ids).Order("cdn_source").Find(&ts)
	for _, t := range ts {
		out[t.CDNProfileID] = append(out[t.CDNProfileID], cdnProbeView{Port: t.CDNPort, Source: t.CDNSource, TokenID: t.ID, Prefix: t.Prefix,
			CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, LastIP: t.LastIP})
	}
	return out
}

// pairCDNProbe is POST /cdn-profiles/:id/probes {source}: a token for one
// speed-test point. Pairing a name again replaces its old token.
func (s *Server) pairCDNProbe(c *gin.Context) {
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if cdnScheme(&p) == "http" {
		badRequest(c, errCDNProbeHTTP)
		return
	}
	var req struct {
		Port   int    `json:"port"`
		Source string `json:"source"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	source := strings.TrimSpace(req.Source)
	if !cdnSourceRe.MatchString(source) {
		badRequest(c, errors.New("测速点名称例如「上海电信」：字母、数字、汉字开头，最长 40 个字符"))
		return
	}
	name := "cf-ip-probe · " + source
	for len(name) > 64 { // the token name column holds 64 bytes
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	var tok *store.APIToken
	var secret string
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		if err := tx.First(&p, "id = ?", p.ID).Error; err != nil {
			return err
		}
		if cdnScheme(&p) == "http" {
			return errCDNProbeHTTP
		}
		if req.Port == 0 {
			req.Port = cdnDefaultPort(&p)
		}
		if req.Port < 1 || req.Port > 65535 || (p.Provider == "cloudflare" && !slices.Contains(cdnClientPorts(&p), req.Port)) {
			return cdnSchemeProblem("测速端口不属于当前 CDN 协议的可用端口")
		}
		if err := tx.Where("cdn_profile_id = ? AND cdn_source = ?", p.ID, source).Delete(&store.APIToken{}).Error; err != nil {
			return err
		}
		t, sec, err := s.issueAPITokenIn(tx, name, "", []string{ScopeCDNIPs}, 0, c.GetString("admin"), func(t *store.APIToken) {
			t.CDNProfileID, t.CDNSource, t.CDNPort = p.ID, source, req.Port
		})
		tok, secret = t, sec
		return err
	})
	if err != nil {
		if _, validation := err.(cdnSchemeProblem); errors.Is(err, errCDNProbeHTTP) || validation {
			badRequest(c, err)
		} else {
			serverError(c, err)
		}
		return
	}
	s.auditCtx(c, "cdn.probe_pair", p.Name, source)
	c.JSON(http.StatusCreated, gin.H{"port": req.Port, "source": source, "tokenId": tok.ID, "token": secret,
		"mirror": strings.TrimRight(s.st.GetSetting("github_mirror", ""), "/")})
}

// cdnTokenAllows checks a paired point's token against the profile and
// source of a report. Ordinary cdn:ips:write tokens may report anywhere.
func cdnTokenAllows(t *store.APIToken, profileID, source string) error {
	if t == nil || t.CDNProfileID == "" {
		return nil
	}
	if t.CDNProfileID != profileID {
		return errors.New("这个令牌属于另一个 CDN 配置档的测速点")
	}
	if t.CDNSource != "" && t.CDNSource != source {
		return errors.New("这个令牌只能以测速点「" + t.CDNSource + "」的名义上报")
	}
	return nil
}
