package agent

import (
	"net"
	"strconv"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// No core management API may sit on the agent's own auth listener: GOST used
// base+5 = 10090 and failed with "address already in use".
func TestCoreAPIPortsAvoidAuthListener(t *testing.T) {
	// The defaults New applies.
	a := &Agent{cfg: Config{AuthListen: "127.0.0.1:10090", APIPortBase: 10085}}
	seen := map[string]model.CoreType{a.cfg.AuthListen: "auth"}
	for ct := range apiOffsets {
		l := a.apiListen(ct)
		if other, dup := seen[l]; dup {
			t.Fatalf("%s API %s collides with %s", ct, l, other)
		}
		seen[l] = ct
	}
	_, p, _ := net.SplitHostPort(a.apiListen(model.CoreGost))
	if n, _ := strconv.Atoi(p); n != a.cfg.APIPortBase+6 {
		t.Fatalf("gost API port %s", p)
	}
}

// realm publishes no digest for 2.7.0; the pinned sums must cover both arches
// without asking the GitHub API.
func TestForwarderPinnedDigest(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		old := goarchFn
		goarchFn = func() string { return arch }
		r, err := releaseFor(model.CoreForwarder, "")
		goarchFn = old
		if err != nil {
			t.Fatal(err)
		}
		d, err := r.digest(t.Context(), &Installer{}, r.url)
		if err != nil || len(d) != 64 {
			t.Fatalf("%s: digest %q %v", arch, d, err)
		}
	}
}
