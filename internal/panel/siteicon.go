package panel

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// siteIcon shares the built-in artwork with the user center and subscription
// pages, which live outside the configurable admin path.
func (s *Server) siteIcon(c *gin.Context) {
	if s.webFS == nil {
		c.Status(http.StatusNotFound)
		return
	}
	f, err := s.webFS.Open("/apple-touch-icon.png")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Type", "image/png")
	c.Header("Cache-Control", "public, max-age=3600")
	c.Header("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(data)))
	http.ServeContent(c.Writer, c.Request, "polaris-icon.png", time.Time{}, bytes.NewReader(data))
}
