package agent

import (
	"net"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestPortConflicts(t *testing.T) {
	// Something else holds a TCP port and a UDP port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	busyTCP := l.Addr().(*net.TCPAddr).Port
	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	busyUDP := u.LocalAddr().(*net.UDPAddr).Port
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()

	vless := func(tag string, port int) model.Inbound {
		return model.Inbound{Tag: tag, Port: port, Listen: "127.0.0.1", Enabled: true,
			Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Transport: model.TransportSpec{Type: model.TransTCP}}
	}
	hy := model.Inbound{Tag: "hy2", Port: busyUDP, Listen: "127.0.0.1", Enabled: true, Protocol: model.ProtocolSpec{Type: model.ProtoHysteria2}}
	next := &model.NodeSpec{Inbounds: []model.Inbound{vless("reality", busyTCP), vless("ok", freePort), hy}}

	msgs := portConflicts(nil, next)
	joined := strings.Join(msgs, "\n")
	if len(msgs) != 2 || !strings.Contains(joined, "reality") || !strings.Contains(joined, "hy2") || strings.Contains(joined, "「ok」") {
		t.Fatalf("conflicts: %v", msgs)
	}
	// Ports our own cores already hold (the applied spec) are not conflicts.
	if msgs := portConflicts(next, next); len(msgs) != 0 {
		t.Fatalf("own ports reported: %v", msgs)
	}
	// Disabled inbounds open nothing.
	off := vless("off", busyTCP)
	off.Enabled = false
	if msgs := portConflicts(nil, &model.NodeSpec{Inbounds: []model.Inbound{off}}); len(msgs) != 0 {
		t.Fatalf("disabled inbound reported: %v", msgs)
	}
}
