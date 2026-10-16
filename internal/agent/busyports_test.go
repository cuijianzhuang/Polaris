package agent

import (
	"net"
	"testing"
)

func TestBusyPorts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	want := l.Addr().(*net.TCPAddr).Port
	tcp, _ := busyPorts()
	for _, p := range tcp {
		if p == want {
			return
		}
	}
	t.Fatalf("listening port %d not reported in %v", want, tcp)
}
