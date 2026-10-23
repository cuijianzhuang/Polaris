package model

import "testing"

func TestLimitBaseUser(t *testing.T) {
	for _, in := range []string{"v4", "v6", "auto"} {
		wire := ScopedLimitUser("alice", in)
		if wire == "alice" || LimitBaseUser(wire) != "alice" {
			t.Fatal(wire)
		}
	}
	for _, in := range []string{"alice", "alice#lim", "alice#ip:bad", "alice#ip:123456789012345z"} {
		if LimitBaseUser(in) != in {
			t.Fatal(in)
		}
	}
	if ScopedLimitUser("alice", "v4") == ScopedLimitUser("alice", "v6") {
		t.Fatal("context collision")
	}
}
