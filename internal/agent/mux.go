package agent

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// SNI mux: the agent listens on a mux group's public port and passes each
// TLS connection, unmodified, to the member inbound whose SNI the
// ClientHello carries (members terminate TLS themselves on loopback).
// The splitter never terminates TLS and holds no private keys.
//
// Hot-path guards (design red lines, batch 1): a ClientHello read
// deadline, a per-listener connection cap, and reaping of half-open
// connections (the deadline also bounds how long a silent connection
// can hold a slot).

var (
	muxHelloTimeout = 10 * time.Second // tests shrink this
	muxDialTimeout  = 5 * time.Second
	muxMaxConns     = 2048 // per listener
)

// maxClientHello caps the buffered handshake bytes: a ClientHello is a
// few KB even with large extension sets; 64 KiB is generous headroom.
const maxClientHello = 64 << 10

type muxTable struct {
	routes   map[string]string // lower-case SNI -> upstream host:port
	fallback string            // upstream for unmatched traffic, "" = drop
}

type muxListener struct {
	port   int
	listen string
	name   string
	ln     net.Listener
	table  atomic.Value // muxTable
	sem    chan struct{}
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

func (l *muxListener) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	conns := l.conns
	l.conns = map[net.Conn]struct{}{}
	l.mu.Unlock()
	_ = l.ln.Close()
	for c := range conns {
		_ = c.Close()
	}
}

func (l *muxListener) track(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.conns[c] = struct{}{}
	return true
}

func (l *muxListener) untrack(c net.Conn) {
	l.mu.Lock()
	delete(l.conns, c)
	l.mu.Unlock()
}

func (l *muxListener) serve(log *slog.Logger) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return // listener closed
		}
		select {
		case l.sem <- struct{}{}:
		default:
			_ = c.Close() // at capacity: refuse rather than queue unbounded
			continue
		}
		if !l.track(c) {
			<-l.sem
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-l.sem }()
			defer l.untrack(c)
			l.handle(c, log)
		}()
	}
}

// handle peeks at the ClientHello, picks the upstream, then proxies the
// buffered bytes plus the rest of the stream in both directions.
func (l *muxListener) handle(client net.Conn, log *slog.Logger) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(muxHelloTimeout))
	sni, prefix, isTLS, err := readClientHelloSNI(client)
	if err != nil {
		return // truncated / malformed handshake or silent peer: drop
	}
	_ = client.SetReadDeadline(time.Time{})
	tbl, _ := l.table.Load().(muxTable)
	upstream := ""
	if isTLS && sni != "" {
		upstream = tbl.routes[sni]
	}
	if upstream == "" {
		// No SNI, unknown SNI and non-TLS traffic all take the group's
		// default target (design D6); without one they are dropped.
		upstream = tbl.fallback
	}
	if upstream == "" {
		return
	}
	srv, err := net.DialTimeout("tcp", upstream, muxDialTimeout)
	if err != nil {
		log.Debug("mux dial failed", "group", l.name, "upstream", upstream, "err", err)
		return
	}
	defer srv.Close()
	l.mu.Lock()
	l.conns[srv] = struct{}{}
	l.mu.Unlock()
	defer l.untrack(srv)
	if len(prefix) > 0 {
		if _, err := srv.Write(prefix); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(srv, client)
	go cp(client, srv)
	<-done
}

// muxManager owns the node's SNI mux listeners, reconciled per spec.
type muxManager struct {
	mu        sync.Mutex
	log       *slog.Logger
	listeners map[int]*muxListener
}

func newMuxManager(log *slog.Logger) *muxManager {
	return &muxManager{log: log, listeners: map[int]*muxListener{}}
}

// wantedMuxes indexes a spec's mux groups by port.
func wantedMuxes(spec *model.NodeSpec) map[int]model.MuxSpec {
	want := map[int]model.MuxSpec{}
	if spec != nil {
		for _, mx := range spec.Muxes {
			want[mx.Port] = mx
		}
	}
	return want
}

func muxTableOf(mx model.MuxSpec) muxTable {
	tbl := muxTable{routes: map[string]string{}, fallback: mx.Fallback}
	for _, r := range mx.Routes {
		tbl.routes[strings.ToLower(r.SNI)] = r.Upstream
	}
	return tbl
}

// closeStale closes listeners the next spec no longer wants on that port
// (or wants on a different listen address), freeing the port before the
// cores rebind — e.g. a group was deleted and a member now owns the port
// itself.
func (m *muxManager) closeStale(next *model.NodeSpec) {
	want := wantedMuxes(next)
	m.mu.Lock()
	defer m.mu.Unlock()
	for port, l := range m.listeners {
		w, ok := want[port]
		if !ok || w.Listen != l.listen {
			l.close()
			delete(m.listeners, port)
		}
	}
}

// sync reconciles listeners with the spec: stale ones close, surviving
// ones swap their routing table in place, new ones bind.
func (m *muxManager) sync(spec *model.NodeSpec) error {
	m.closeStale(spec)
	want := wantedMuxes(spec)
	ports := make([]int, 0, len(want))
	for p := range want {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []string
	for _, port := range ports {
		w := want[port]
		tbl := muxTableOf(w)
		if l, ok := m.listeners[port]; ok {
			l.table.Store(tbl)
			l.name = w.Name
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(w.Listen, strconv.Itoa(port)))
		if err != nil {
			errs = append(errs, fmt.Sprintf("SNI 复用组「%s」监听 %s:%d 失败：%v", w.Name, w.Listen, port, err))
			continue
		}
		l := &muxListener{port: port, listen: w.Listen, name: w.Name, ln: ln,
			sem: make(chan struct{}, muxMaxConns), conns: map[net.Conn]struct{}{}}
		l.table.Store(tbl)
		m.listeners[port] = l
		go l.serve(m.log)
		m.log.Info("mux listener up", "group", w.Name, "port", port, "routes", len(w.Routes))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// readClientHelloSNI reads the first TLS record(s) from r and extracts
// the ClientHello SNI. prefix receives every byte read, to replay to the
// upstream. isTLS reports whether the stream began with a TLS handshake
// record at all; non-TLS streams are not an error (the caller routes
// them to the fallback with prefix replayed). A structurally broken or
// truncated handshake is an error: the caller drops the connection.
func readClientHelloSNI(r io.Reader) (sni string, prefix []byte, isTLS bool, err error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return "", nil, false, err
	}
	prefix = append(prefix, hdr...)
	if hdr[0] != 0x16 { // not a TLS handshake record
		return "", prefix, false, nil
	}
	var handshake []byte
	for {
		recLen := int(hdr[3])<<8 | int(hdr[4])
		if recLen <= 0 || recLen > 18432 { // RFC 8446: 2^14 + 2048 max
			return "", nil, true, errors.New("tls record too large")
		}
		payload := make([]byte, recLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", nil, true, err
		}
		prefix = append(prefix, payload...)
		handshake = append(handshake, payload...)
		if len(handshake) > maxClientHello {
			return "", nil, true, errors.New("client hello too large")
		}
		if len(handshake) >= 4 {
			msgLen := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
			if len(handshake) >= 4+msgLen {
				break // full handshake message buffered
			}
		}
		// The ClientHello spans further records: read the next header.
		if _, err := io.ReadFull(r, hdr); err != nil {
			return "", nil, true, err
		}
		prefix = append(prefix, hdr...)
		if hdr[0] != 0x16 {
			return "", nil, true, errors.New("handshake record expected")
		}
	}
	name, perr := parseClientHelloSNI(handshake)
	if perr != nil {
		return "", nil, true, perr
	}
	return name, prefix, true, nil
}

// parseClientHelloSNI walks a complete ClientHello handshake message
// (type byte + 3-byte length + body) and returns its server_name, or ""
// when the extension is absent.
func parseClientHelloSNI(b []byte) (string, error) {
	if len(b) < 4 || b[0] != 0x01 {
		return "", errors.New("not a client hello")
	}
	body := b[4:]
	rd := &byteReader{b: body}
	rd.take(2 + 32) // version + random
	if err := rd.err; err != nil {
		return "", err
	}
	rd.skipLen8()  // session id
	rd.skipLen16() // cipher suites
	rd.skipLen8()  // compression methods
	if rd.err != nil {
		return "", rd.err
	}
	exts := rd.takeLen16() // extensions block
	if rd.err != nil {
		return "", nil // a hello without extensions carries no SNI
	}
	er := &byteReader{b: exts}
	for {
		hdr := er.take(4)
		if er.err != nil {
			return "", nil // walked everything: no server_name found
		}
		typ := int(hdr[0])<<8 | int(hdr[1])
		ln := int(hdr[2])<<8 | int(hdr[3])
		data := er.take(ln)
		if er.err != nil {
			return "", er.err
		}
		if typ != 0 { // server_name
			continue
		}
		sr := &byteReader{b: data}
		list := sr.takeLen16()
		if sr.err != nil {
			return "", sr.err
		}
		lr := &byteReader{b: list}
		for {
			e := lr.take(3)
			if lr.err != nil {
				return "", nil
			}
			nameLen := int(e[1])<<8 | int(e[2])
			name := lr.take(nameLen)
			if lr.err != nil {
				return "", lr.err
			}
			if e[0] == 0 { // host_name
				return strings.ToLower(string(name)), nil
			}
		}
	}
}

// byteReader is a bounds-checked cursor over a byte slice.
type byteReader struct {
	b   []byte
	err error
}

func (r *byteReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b) < n {
		r.err = errors.New("client hello truncated")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *byteReader) skipLen8() {
	if r.err != nil {
		return
	}
	h := r.take(1)
	if r.err == nil {
		r.take(int(h[0]))
	}
}

func (r *byteReader) skipLen16() {
	if r.err != nil {
		return
	}
	h := r.take(2)
	if r.err == nil {
		r.take(int(h[0])<<8 | int(h[1]))
	}
}

func (r *byteReader) takeLen16() []byte {
	if r.err != nil {
		return nil
	}
	h := r.take(2)
	if r.err != nil {
		return nil
	}
	return r.take(int(h[0])<<8 | int(h[1]))
}
