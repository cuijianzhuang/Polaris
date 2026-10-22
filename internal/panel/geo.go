package panel

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/geodata"
)

// geoCategories is GET /routing/geo?type=geosite|geoip&q=: every category
// (the editor filters locally); for geosite a q that looks like a host name
// also returns the categories containing it.
func (s *Server) geoCategories(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if c.DefaultQuery("type", "geosite") == "geoip" {
		ips, err := geodata.IPs()
		if err != nil {
			serverError(c, err)
			return
		}
		type item struct {
			geodata.IPCategory
			SingBox bool `json:"singBox"`
		}
		out := make([]item, len(ips))
		for i, x := range ips {
			out[i] = item{x, geodata.IPSingBox(x.Name)}
		}
		c.JSON(http.StatusOK, gin.H{"items": out})
		return
	}
	cats, err := geodata.Sites()
	if err != nil {
		serverError(c, err)
		return
	}
	out := gin.H{"items": cats}
	if strings.Contains(q, ".") && !strings.ContainsAny(q, " /") {
		hits, _ := geodata.Lookup(q)
		out["lookup"] = nonNil(hits)
	}
	c.JSON(http.StatusOK, out)
}

// geoEntries is GET /routing/geo/geosite/:name: a category's domains
// ("name" or "name@attr"), the first 1000.
func (s *Server) geoEntries(c *gin.Context) {
	ds, err := geodata.Entries(c.Param("name"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	total := len(ds)
	if len(ds) > 1000 {
		ds = ds[:1000]
	}
	c.JSON(http.StatusOK, gin.H{"entries": nonNil(ds), "total": total})
}
