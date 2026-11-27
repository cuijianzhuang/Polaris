package panel

// GitHub Gist as a backup push target: the encrypted archive is stored in a
// single secret Gist (see docs/design/gist-backup.md). This file holds the
// API client shared by the settings "test connection" endpoint and the pro
// build's push implementation (backupgist_pro.go in polaris-pro).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// gistAPIBase is a var so tests can point the client at a local server.
var gistAPIBase = "https://api.github.com"

type gistClient struct {
	token string
}

// do performs one GitHub API request and returns the status and body.
func (g gistClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, gistAPIBase+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "polaris-backup")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// gistError renders a failed GitHub response as a readable error.
func gistError(status int, body []byte) error {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Message != "" {
		return fmt.Errorf("GitHub 返回 %d：%s", status, e.Message)
	}
	return fmt.Errorf("GitHub 返回 %d %s", status, strings.TrimSpace(string(body)))
}

// gistCheck validates the token and, when a Gist id is saved, that the Gist
// is still readable with it.
func gistCheck(ctx context.Context, token, gistID string) error {
	cl := gistClient{token: token}
	status, body, err := cl.do(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return gistError(status, body)
	}
	if gistID != "" {
		status, body, err = cl.do(ctx, http.MethodGet, "/gists/"+gistID, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("已保存的 Gist 不可读：%w", gistError(status, body))
		}
	}
	return nil
}

// backupGistTest: POST /backup/gist/test {token} checks the Gist settings
// before saving them; an empty token means the saved one.
func (s *Server) backupGistTest(c *gin.Context) {
	var b struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		badRequest(c, err)
		return
	}
	token := b.Token
	if token == "" {
		token = s.st.Box.MustOpen(s.st.GetSetting("backup_gist_token", ""))
	}
	if token == "" {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "请先填写 GitHub 令牌（需要 gist 权限）"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := gistCheck(ctx, token, s.st.GetSetting("backup_gist_id", "")); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
