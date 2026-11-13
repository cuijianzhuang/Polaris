package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
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

// upgradeAgentAPI starts a one-node agent upgrade as a rollout and returns
// it at once. The upgrade itself (fetching the binary, the node's download
// and restart) takes minutes, far longer than any reverse proxy waits for
// one HTTP response, so a handler that blocks on it is killed mid-flight
// and the browser reports a network failure ("Load failed"). The rollout
// runs in the background; the panel polls /rollouts for its progress.
func (s *Server) upgradeAgentAPI(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if n.AgentVersion == Version && Version != "dev" {
		c.JSON(http.StatusOK, gin.H{"version": Version})
		return
	}
	r, err := s.newRollout(rolloutReq{Kind: "agent", NodeIDs: []string{n.ID}, Canary: 1, BatchSize: 1, SoakSeconds: 30}, c.GetString("admin"))
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "node.upgrade_agent", n.Name, Version)
	go s.runRollouts() // start now rather than on the next tick
	c.JSON(http.StatusOK, r)
}

// upgradeAllAgents starts a one-batch agent rollout for every outdated
// node and returns it at once; see upgradeAgentAPI for why it must not
// block on the upgrades.
func (s *Server) upgradeAllAgents(c *gin.Context) {
	ids, err := s.rolloutNodes(rolloutReq{Kind: "agent", Version: rolloutAgentVersion()})
	if err != nil {
		dbError(c, err)
		return
	}
	if len(ids) == 0 {
		c.JSON(http.StatusOK, gin.H{"version": Version, "results": []agentUpgradeResult{}})
		return
	}
	n := min(len(ids), 20)
	r, err := s.newRollout(rolloutReq{Kind: "agent", NodeIDs: ids, Canary: n, BatchSize: n, SoakSeconds: 30}, c.GetString("admin"))
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "node.upgrade_agent_all", fmt.Sprintf("%d", len(r.NodeIDs.V)), Version)
	go s.runRollouts() // start now rather than on the next tick
	c.JSON(http.StatusOK, r)
}
