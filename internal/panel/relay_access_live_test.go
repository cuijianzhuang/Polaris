package panel

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/forwarder"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestLiveRelayProxyProtocol(t *testing.T) {
	bin := os.Getenv("POLARIS_REALM_BIN")
	if bin == "" {
		t.Skip("POLARIS_REALM_BIN not set")
	}
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			s, c, id := relayAccessFixture(t)
			ln, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer ln.Close()
			port := ln.Addr().(*net.TCPAddr).Port
			s.st.DB.Model(&store.Node{}).Where("id IN ?", []string{"relay", "middle", "land"}).Update("address", "127.0.0.1")
			s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("port", port)
			if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true, "proxyProtocol": version}); code != 200 {
				t.Fatal(code, out)
			}
			got := make(chan string, 1)
			go func() {
				for {
					conn, e := ln.Accept()
					if e != nil {
						return
					}
					go func() {
						defer conn.Close()
						conn.SetDeadline(time.Now().Add(2 * time.Second))
						reader := bufio.NewReader(conn)
						source := ""
						if version == 1 {
							line, e := reader.ReadString('\n')
							if e != nil {
								return
							}
							parts := strings.Fields(line)
							if len(parts) != 6 || parts[0] != "PROXY" {
								return
							}
							source = net.ParseIP(parts[2]).String()
						} else {
							header := make([]byte, 16)
							if _, e := io.ReadFull(reader, header); e != nil {
								return
							}
							if string(header[:12]) != "\r\n\r\n\x00\r\nQUIT\n" || (header[13] != 0x11 && header[13] != 0x21) {
								return
							}
							addr := make([]byte, int(binary.BigEndian.Uint16(header[14:16])))
							if len(addr) < 12 {
								return
							}
							if _, e := io.ReadFull(reader, addr); e != nil {
								return
							}
							if header[13] == 0x21 {
								if len(addr) < 36 {
									return
								}
								source = net.IP(addr[:16]).String()
							} else {
								source = net.IP(addr[:4]).String()
							}
						}
						payload := make([]byte, 5)
						if _, e := io.ReadFull(reader, payload); e != nil || string(payload) != "HELLO" {
							return
						}
						select {
						case got <- source:
						default:
						}
						conn.Write([]byte("verified"))
					}()
				}
			}()
			for _, nodeID := range []string{"middle", "relay"} {
				spec, e := s.BuildSpec(nodeID)
				if e != nil {
					t.Fatal(e)
				}
				dir := t.TempDir()
				ad := forwarder.New(core.Options{Version: "2.9.6", BinPath: bin, WorkDir: dir})
				cfg, e := ad.Render(spec)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(dir, "realm.toml"), cfg, 0600); e != nil {
					t.Fatal(e)
				}
				if e = ad.Start(t.Context()); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { ad.Stop(context.Background()) })
			}
			var chain store.Chain
			s.st.DB.First(&chain, "id = ?", id)
			dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.7")}}
			verified := false
			for end := time.Now().Add(8 * time.Second); time.Now().Before(end); {
				conn, e := dialer.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", chain.Hops.V[0].Port))
				if e == nil {
					conn.SetDeadline(time.Now().Add(time.Second))
					conn.Write([]byte("HELLO"))
					b := make([]byte, 8)
					_, e = io.ReadFull(conn, b)
					conn.Close()
					if e == nil && string(b) == "verified" {
						verified = true
						break
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			if !verified {
				t.Fatal("two-hop PROXY forwarding failed")
			}
			select {
			case source := <-got:
				if source != "127.0.0.7" {
					t.Fatalf("original client IP lost: %s", source)
				}
			case <-time.After(time.Second):
				t.Fatal("no backend source captured")
			}
		})
	}
}
