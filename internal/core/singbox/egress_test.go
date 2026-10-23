package singbox

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
	bin := os.Getenv("POLARIS_SINGBOX_BIN")
	if bin == "" {
		t.Skip("POLARIS_SINGBOX_BIN not set")
	}
	dir := t.TempDir()
	a := New(core.Options{Version: "1.13.5", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
	coretest.LiveOutboundIP(t, a, model.CoreSingbox, filepath.Join(dir, "singbox.json"), nil)
}
