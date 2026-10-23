package xray

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestLiveOutboundIP(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_EGRESS_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_EGRESS_BIN not set")
	}
	dir := t.TempDir()
	a := New(core.Options{Version: "26.5.9", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", freePort(t))})
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range a.Logs(150) {
				t.Log(line)
			}
		}
	})
	coretest.LiveOutboundIP(t, a, model.CoreXray, filepath.Join(dir, "xray.json"), nil)
}

func TestLiveOutboundIPBalancerFallback(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_EGRESS_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_EGRESS_BIN not set")
	}
	dir := t.TempDir()
	a := New(core.Options{Version: "26.5.9", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", freePort(t))})
	coretest.LiveBalancerFallback(t, a, model.CoreXray, filepath.Join(dir, "xray.json"), model.LBRoundRobin, model.OutboundPreferIPv6)
}
