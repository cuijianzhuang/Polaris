package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

type fakeAccessNFT struct {
	exists, failCheck, failApply bool
	script                       string
	log                          *callLog
}

func (n *fakeAccessNFT) run(_ context.Context, name string, args ...string) (string, error) {
	if name != "nft" {
		return "", errors.New("unexpected command")
	}
	if args[0] == "list" {
		if n.exists {
			return "table inet polaris_access", nil
		}
		return "No such file or directory", errors.New("missing")
	}
	b, e := os.ReadFile(args[len(args)-1])
	if e != nil {
		return "", e
	}
	if args[0] == "--check" {
		if n.failCheck {
			return "bad syntax", errors.New("check")
		}
		return "", nil
	}
	if n.failApply {
		return "transaction failed", errors.New("apply")
	}
	n.script = string(b)
	n.exists = strings.Contains(n.script, "table inet polaris_access {")
	if n.log != nil {
		n.log.add("access:commit")
	}
	return "", nil
}

func TestAccessFirewallFailures(t *testing.T) {
	f := newFirewall(t.TempDir())
	n := &fakeAccessNFT{}
	f.look = func(name string) bool { return name == "nft" }
	f.run = n.run
	rules := []model.AccessRule{{Protocol: "tcp", Port: 443, Sources: []string{"198.51.100.1/32", "2001:db8::1/128"}}}
	if e := f.syncAccess(context.Background(), rules); e != nil {
		t.Fatal(e)
	}
	before := n.script
	for _, kind := range []string{"check", "apply"} {
		n.failCheck = kind == "check"
		n.failApply = kind == "apply"
		next := []model.AccessRule{{Protocol: "tcp", Port: 8443, Sources: []string{"203.0.113.1/32"}}}
		if e := f.syncAccess(context.Background(), next); e == nil {
			t.Fatal("failure accepted", kind)
		}
		if n.script != before {
			t.Fatal("failure modified active rule set")
		}
		stored, e := f.accessState()
		if e != nil || len(stored) != 1 || stored[0].Port != 443 {
			t.Fatal(stored, e)
		}
	}
	n.failCheck, n.failApply = false, false
	if e := f.syncAccess(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
	if n.exists || strings.Contains(n.script, "flush ruleset") {
		t.Fatal("cleanup changed unrelated rules", n.script)
	}
}

func TestAccessBlocksApplyWithoutEnforcement(t *testing.T) {
	log := &callLog{}
	fake := newFake(model.CoreXray, log, false)
	a := newFakeAgent(t, fake)
	a.fw = newFirewall(t.TempDir())
	a.fw.look = func(string) bool { return false }
	spec := &model.NodeSpec{Rev: 1, Cores: []model.CoreSpec{{Type: model.CoreXray}}, Inbounds: []model.Inbound{vlessIn(model.CoreXray, "in", freePortT(t))}, AccessRules: []model.AccessRule{{Protocol: "tcp", Port: 443}}}
	r := a.Apply(context.Background(), spec)
	if r.ok || r.err == nil || a.appliedRev() != 0 || len(log.all()) != 0 {
		t.Fatalf("unenforced access reached core: %+v %v", r, log.all())
	}
}

func TestAccessGuardsBeforeCoreAndFailure(t *testing.T) {
	log := &callLog{}
	fake := newFake(model.CoreXray, log, false)
	a := newFakeAgent(t, fake)
	a.fw = newFirewall(t.TempDir())
	n := &fakeAccessNFT{log: log}
	a.fw.look = func(name string) bool { return name == "nft" }
	a.fw.run = n.run
	in := vlessIn(model.CoreXray, "in", freePortT(t))
	spec := &model.NodeSpec{NodeID: "n", Rev: 1, Cores: []model.CoreSpec{{Type: model.CoreXray}}, Inbounds: []model.Inbound{in}, AccessRules: []model.AccessRule{{Protocol: "tcp", Port: in.Port, Sources: []string{"198.51.100.1/32"}}}}
	if r := a.Apply(context.Background(), spec); !r.ok {
		t.Fatal(r.err)
	}
	if calls := log.all(); len(calls) == 0 || calls[0] != "access:commit" {
		t.Fatalf("core started before protection: %v", calls)
	}
	next := spec.Clone()
	next.Rev = 2
	next.Inbounds[0].Tag = "changed"
	next.AccessRules[0].Sources = []string{"203.0.113.1/32"}
	fake.setFailValidate(true)
	if r := a.Apply(context.Background(), next); r.ok {
		t.Fatal("core failure accepted")
	}
	stored, e := a.fw.accessState()
	if e != nil || len(stored) != 1 || len(stored[0].Sources) != 0 || a.appliedRev() != 1 {
		t.Fatalf("failure did not retain deny-all transition: %+v %v", stored, e)
	}
	fake.setFailValidate(false)
	next.Rev = 3
	if r := a.Apply(context.Background(), next); !r.ok {
		t.Fatal(r.err)
	}
	stored, e = a.fw.accessState()
	if e != nil || len(stored[0].Sources) != 1 || stored[0].Sources[0] != "203.0.113.1/32" {
		t.Fatal(stored, e)
	}
}

// Exercise a core refusing to stop while its source restriction is removed.
type refuseStopCore struct{ *fakeCore }

func (f *refuseStopCore) Stop(context.Context) error { return errors.New("still running") }

func TestAccessRetainedWhenCoreCannotStop(t *testing.T) {
	log := &callLog{}
	fake := newFake(model.CoreXray, log, false)
	a := newFakeAgent(t, fake)
	a.fw = newFirewall(t.TempDir())
	n := &fakeAccessNFT{}
	a.fw.look = func(name string) bool { return name == "nft" }
	a.fw.run = n.run
	in := vlessIn(model.CoreXray, "in", freePortT(t))
	spec := &model.NodeSpec{Rev: 1, Cores: []model.CoreSpec{{Type: model.CoreXray}}, Inbounds: []model.Inbound{in}, AccessRules: []model.AccessRule{{Protocol: "tcp", Port: in.Port}}}
	if r := a.Apply(context.Background(), spec); !r.ok {
		t.Fatal(r.err)
	}
	a.cores[model.CoreXray].adapter = &refuseStopCore{fake}
	if r := a.Apply(context.Background(), &model.NodeSpec{Rev: 2}); r.ok {
		t.Fatal("stop failure acknowledged")
	}
	guard, e := a.fw.accessState()
	if e != nil || len(guard) != 1 || a.appliedRev() != 1 || a.cores[model.CoreXray] == nil {
		t.Fatalf("guard removed before listener stopped: %+v %v", guard, e)
	}
	a.cores[model.CoreXray].adapter = fake
}
