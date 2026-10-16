package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// One-click agent upgrades. The panel sends an upgrade_agent command with
// the target version and the SHA-256 of each architecture's binary; the
// agent fetches its binary over the mTLS agent endpoint it is already
// connected to (so no extra port or GitHub access is needed), verifies it,
// swaps it in and re-executes itself.

const upgradeTimeout = 3 * time.Minute

// serveAgentBinary is GET /agent/binary/{arch} on the agent endpoint. Only
// enrolled, non-revoked nodes may download.
func (s *Server) serveAgentBinary(w http.ResponseWriter, r *http.Request) {
	if _, err := (&agentService{s: s}).node(r.Context()); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	arch := strings.TrimPrefix(r.URL.Path, "/agent/binary/")
	if arch != "amd64" && arch != "arm64" {
		http.NotFound(w, r)
		return
	}
	if err := s.ensureAgent(r.Context(), arch); err != nil && !s.agentCurrent(arch) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	p := s.agentPath(arch)
	if _, err := os.Stat(p); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, p)
}

// upgradeAgent upgrades one node and waits for the result of the swap.
func (s *Server) upgradeAgent(ctx context.Context, n *store.Node) error {
	if !s.hub.Online(n.ID) {
		return errors.New("节点不在线")
	}
	if n.AgentVersion == Version && Version != "dev" {
		return nil
	}
	// Fetch the build this node runs on (all of them for agents too old to
	// report their architecture); the others go along if already cached.
	need := agentArches
	if validArch(n.Arch) {
		need = []string{n.Arch}
	}
	for _, a := range need {
		if err := s.ensureAgent(ctx, a); err != nil {
			s.log.Warn("cache agent for upgrade", "arch", a, "err", err)
		}
	}
	args := map[string]string{"version": Version}
	for _, a := range agentArches {
		args["sha256_"+a] = s.agentSHA(a)
	}
	if args["sha256_amd64"] == "" && args["sha256_arm64"] == "" {
		return errors.New("主控上没有可分发的 Agent（等待自动缓存完成，或检查与 GitHub 的连接 / github_mirror）")
	}
	res, err := s.hub.Command(ctx, n.ID, "upgrade_agent", args, upgradeTimeout)
	if err != nil {
		return err
	}
	if !res.Ok {
		return errors.New(res.Error)
	}
	return nil
}

func (s *Server) upgradeAgentAPI(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := s.upgradeAgent(c.Request.Context(), &n); err != nil {
		badRequest(c, fmt.Errorf("升级失败：%w", err))
		return
	}
	s.auditCtx(c, "node.upgrade_agent", n.Name, Version)
	c.JSON(http.StatusOK, gin.H{"version": Version})
}

// upgradeAllAgents upgrades every online node running another version.
func (s *Server) upgradeAllAgents(c *gin.Context) {
	var nodes []store.Node
	s.st.DB.Find(&nodes)
	type result struct {
		Node  string `json:"node"`
		Ok    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = []result{}
		sem = make(chan struct{}, 4)
	)
	for i := range nodes {
		n := &nodes[i]
		if !s.hub.Online(n.ID) || n.AgentVersion == Version {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := s.upgradeAgent(c.Request.Context(), n)
			r := result{Node: n.Name, Ok: err == nil}
			if err != nil {
				r.Error = err.Error()
			}
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	s.auditCtx(c, "node.upgrade_agent_all", fmt.Sprintf("%d", len(out)), Version)
	c.JSON(http.StatusOK, gin.H{"version": Version, "results": out})
}
