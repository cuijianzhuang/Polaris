package agent

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Per-user speed limits. Cores send a limited user's traffic to the
// limiter over SOCKS5 (username = user id); the limiter shapes it with the
// user's token buckets and hands it back to the same core's re-entry inbound
// as "<id>#lim", where the normal routing continues. Users not limited never
// pass through here. One bucket pair per user is shared by every core on the
// node, so a user's limit covers all of their connections.

type limiter struct {
	log    *slog.Logger
	secret string

	mu      sync.Mutex
	buckets map[string]*userBucket
	servers map[string]*limitServer // by listen address

	statAt   time.Time
	lastSeen map[string][2]int64 // byte counters at the previous stats call
}

type userBucket struct {
	up, down *rate.Limiter
	conns    int
	// bytes passed each way, for the speed the heartbeat reports
	upBytes, downBytes atomic.Int64
	limited            bool // listed in the spec's limits
}

// limitStat is one limited user's allowance and current speed.
type limitStat struct {
	User             string
	UpKbps, DownKbps int   // allowance (0 = unlimited)
	UpBps, DownBps   int64 // measured since the previous stats call
}

// maxUserConns caps one limited user's concurrent connections through the
// limiter (file descriptors are shared with the cores).
const maxUserConns = 1024

func newLimiter(dataDir string, log *slog.Logger) *limiter {
	return &limiter{log: log, secret: limiterSecret(dataDir), buckets: map[string]*userBucket{}, servers: map[string]*limitServer{}}
}

// limiterSecret is the SOCKS password between the cores and the limiter,
// kept across restarts so rendered configs stay the same.
func limiterSecret(dataDir string) string {
	p := filepath.Join(dataDir, "agent", "limiter.key")
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b))
	}
	k := make([]byte, 16)
	rand.Read(k)
	s := hex.EncodeToString(k)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(s+"\n"), 0o600)
	return s
}

// bytesPerSec turns kbit/s into a limit (0 = unlimited).
func bytesPerSec(kbps int) rate.Limit {
	if kbps <= 0 {
		return rate.Inf
	}
	return rate.Limit(float64(kbps) * 1000 / 8)
}

// burstFor allows a fifth of a second at full rate, at least 64 KiB, so
// short bursts pass and averages stay close to the limit.
func burstFor(l rate.Limit) int {
	if l == rate.Inf {
		return 1 << 20
	}
	return max(int(float64(l)/5), 64<<10)
}

func setRate(r *rate.Limiter, kbps int) {
	l := bytesPerSec(kbps)
	r.SetLimit(l)
	r.SetBurst(burstFor(l))
}

// setLimits applies the users' rates; connections already open follow the
// new rates at once. Users no longer listed become unlimited.
func (l *limiter) setLimits(ls []model.UserLimit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[string]bool{}
	for _, u := range ls {
		seen[u.User] = true
		b := l.buckets[u.User]
		if b == nil {
			b = &userBucket{up: rate.NewLimiter(rate.Inf, 1<<20), down: rate.NewLimiter(rate.Inf, 1<<20)}
			l.buckets[u.User] = b
		}
		setRate(b.up, u.UpKbps)
		setRate(b.down, u.DownKbps)
		b.limited = true
	}
	for id, b := range l.buckets {
		if !seen[id] {
			b.limited = false
			setRate(b.up, 0)
			setRate(b.down, 0)
			if b.conns == 0 {
				delete(l.buckets, id)
			}
		}
	}
}

func (l *limiter) acquire(user string) (*userBucket, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[user]
	if b == nil {
		b = &userBucket{up: rate.NewLimiter(rate.Inf, 1<<20), down: rate.NewLimiter(rate.Inf, 1<<20)}
		l.buckets[user] = b
	}
	if b.conns >= maxUserConns {
		return nil, errors.New("too many connections")
	}
	b.conns++
	return b, nil
}

func (l *limiter) release(user string, b *userBucket) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b.conns--
	if b.conns == 0 && b.up.Limit() == rate.Inf && b.down.Limit() == rate.Inf {
		delete(l.buckets, user)
	}
}

// kbps turns a limiter rate back into kbit/s (0 = unlimited).
func kbps(r *rate.Limiter) int {
	if r.Limit() == rate.Inf {
		return 0
	}
	return int(float64(r.Limit()) * 8 / 1000)
}

// stats reports every limited user's allowance and speed since the last call.
func (l *limiter) stats() []limitStat {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	secs := now.Sub(l.statAt).Seconds()
	first := l.statAt.IsZero()
	l.statAt = now
	seen := map[string][2]int64{}
	var out []limitStat
	for id, b := range l.buckets {
		if !b.limited {
			continue
		}
		cur := [2]int64{b.upBytes.Load(), b.downBytes.Load()}
		seen[id] = cur
		st := limitStat{User: id, UpKbps: kbps(b.up), DownKbps: kbps(b.down)}
		if prev, ok := l.lastSeen[id]; ok && !first && secs > 0 {
			st.UpBps = int64(float64(cur[0]-prev[0]) * 8 / secs)
			st.DownBps = int64(float64(cur[1]-prev[1]) * 8 / secs)
		}
		out = append(out, st)
	}
	l.lastSeen = seen
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

// wire returns how a core reaches the limiter (starting the listener on
// first use) and where its re-entry inbound must listen.
func (l *limiter) wire(listen, reentry string) *model.LimitWire {
	l.mu.Lock()
	srv := l.servers[listen]
	l.mu.Unlock()
	if srv == nil {
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			l.log.Warn("speed limiter unavailable", "listen", listen, "err", err)
			return nil
		}
		srv = &limitServer{l: l, ln: ln, reentry: reentry}
		l.mu.Lock()
		l.servers[listen] = srv
		l.mu.Unlock()
		go srv.serve()
	}
	return &model.LimitWire{Limiter: listen, Reentry: reentry, Secret: l.secret}
}

func (l *limiter) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, s := range l.servers {
		s.ln.Close()
		delete(l.servers, k)
	}
}

// limitServer is the limiter's SOCKS5 endpoint for one core.
type limitServer struct {
	l       *limiter
	ln      net.Listener
	reentry string
}

func (s *limitServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handle(c)
	}
}

const socksTimeout = 10 * time.Second

func (s *limitServer) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(socksTimeout))
	user, err := socksServerAuth(c, s.l.secret)
	if err != nil {
		return
	}
	cmd, addr, err := socksReadRequest(c)
	if err != nil {
		return
	}
	b, err := s.l.acquire(user)
	if err != nil {
		socksReply(c, 0x05, nil)
		return
	}
	defer s.l.release(user, b)
	switch cmd {
	case 0x01: // CONNECT
		s.connect(c, user, addr, b)
	case 0x03: // UDP ASSOCIATE
		s.associate(c, user, b)
	default:
		socksReply(c, 0x07, nil)
	}
}

// dialReentry opens a SOCKS5 session to the core's re-entry inbound as
// "<user>#lim" and sends the command.
func (s *limitServer) dialReentry(user string, cmd byte, addr []byte) (net.Conn, []byte, error) {
	r, err := net.DialTimeout("tcp", s.reentry, socksTimeout)
	if err != nil {
		return nil, nil, err
	}
	r.SetDeadline(time.Now().Add(socksTimeout))
	bnd, err := socksClient(r, user+model.LimitAlias, s.l.secret, cmd, addr)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	r.SetDeadline(time.Time{})
	return r, bnd, nil
}

func (s *limitServer) connect(c net.Conn, user string, addr []byte, b *userBucket) {
	r, _, err := s.dialReentry(user, 0x01, addr)
	if err != nil {
		socksReply(c, 0x01, nil)
		return
	}
	defer r.Close()
	if err := socksReply(c, 0x00, nil); err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { limitedCopy(r, c, b.up, &b.upBytes); closeWrite(r); done <- struct{}{} }()
	go func() { limitedCopy(c, r, b.down, &b.downBytes); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		t.CloseWrite()
	}
}

// limitedCopy copies src to dst at the bucket's rate.
func limitedCopy(dst io.Writer, src io.Reader, lim *rate.Limiter, count *atomic.Int64) {
	buf := make([]byte, 32<<10)
	ctx := context.Background()
	for {
		n := len(buf)
		if lim.Limit() != rate.Inf {
			n = min(n, lim.Burst())
		}
		m, err := src.Read(buf[:n])
		if m > 0 {
			if lim.Limit() != rate.Inf {
				if werr := lim.WaitN(ctx, min(m, lim.Burst())); werr != nil {
					return
				}
			}
			if _, werr := dst.Write(buf[:m]); werr != nil {
				return
			}
			count.Add(int64(m))
		}
		if err != nil {
			return
		}
	}
}

// associate relays a UDP association: datagrams keep their SOCKS5 UDP
// header on both legs, so they pass through unchanged, paced by the buckets.
func (s *limitServer) associate(c net.Conn, user string, b *userBucket) {
	r, bnd, err := s.dialReentry(user, 0x03, []byte{0x01, 0, 0, 0, 0, 0, 0})
	if err != nil {
		socksReply(c, 0x01, nil)
		return
	}
	defer r.Close()
	relay, err := socksAddrUDP(bnd, s.reentry)
	if err != nil {
		socksReply(c, 0x01, nil)
		return
	}
	host, _, _ := net.SplitHostPort(c.LocalAddr().String())
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host)})
	if err != nil {
		socksReply(c, 0x01, nil)
		return
	}
	defer local.Close()
	up, err := net.DialUDP("udp", nil, relay)
	if err != nil {
		socksReply(c, 0x01, nil)
		return
	}
	defer up.Close()
	if err := socksReply(c, 0x00, local.LocalAddr().(*net.UDPAddr)); err != nil {
		return
	}
	c.SetDeadline(time.Time{})

	var mu sync.Mutex
	var client *net.UDPAddr
	go func() { // core -> re-entry (upload)
		buf := make([]byte, 64<<10)
		for {
			n, from, err := local.ReadFromUDP(buf)
			if err != nil {
				return
			}
			mu.Lock()
			client = from
			mu.Unlock()
			if !paceUDP(b.up, n) {
				continue
			}
			up.Write(buf[:n])
			b.upBytes.Add(int64(n))
		}
	}()
	go func() { // re-entry -> core (download)
		buf := make([]byte, 64<<10)
		for {
			n, err := up.Read(buf)
			if err != nil {
				return
			}
			mu.Lock()
			to := client
			mu.Unlock()
			if to == nil || !paceUDP(b.down, n) {
				continue
			}
			local.WriteToUDP(buf[:n], to)
			b.downBytes.Add(int64(n))
		}
	}()
	// The association lives as long as either control connection.
	done := make(chan struct{}, 2)
	go func() { io.Copy(io.Discard, c); done <- struct{}{} }()
	go func() { io.Copy(io.Discard, r); done <- struct{}{} }()
	<-done
}

// paceUDP waits for tokens up to a short delay; a datagram that would wait
// longer is dropped (late UDP is useless and queues add latency).
func paceUDP(lim *rate.Limiter, n int) bool {
	if lim.Limit() == rate.Inf {
		return true
	}
	if n > lim.Burst() {
		n = lim.Burst()
	}
	r := lim.ReserveN(time.Now(), n)
	if !r.OK() {
		return false
	}
	d := r.Delay()
	if d > 200*time.Millisecond {
		r.Cancel()
		return false
	}
	time.Sleep(d)
	return true
}

// --- SOCKS5 wire helpers ---

// socksServerAuth negotiates username/password auth and returns the user.
func socksServerAuth(c net.Conn, secret string) (string, error) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(c, h); err != nil || h[0] != 5 {
		return "", errors.New("not socks5")
	}
	methods := make([]byte, h[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	ok := false
	for _, m := range methods {
		ok = ok || m == 0x02
	}
	if !ok {
		c.Write([]byte{5, 0xff})
		return "", errors.New("no password auth")
	}
	c.Write([]byte{5, 0x02})
	v := make([]byte, 2)
	if _, err := io.ReadFull(c, v); err != nil || v[0] != 1 {
		return "", errors.New("bad auth")
	}
	user := make([]byte, v[1])
	if _, err := io.ReadFull(c, user); err != nil {
		return "", err
	}
	pl := make([]byte, 1)
	if _, err := io.ReadFull(c, pl); err != nil {
		return "", err
	}
	pass := make([]byte, pl[0])
	if _, err := io.ReadFull(c, pass); err != nil {
		return "", err
	}
	if string(pass) != secret || len(user) == 0 {
		c.Write([]byte{1, 1})
		return "", errors.New("bad credentials")
	}
	_, err := c.Write([]byte{1, 0})
	return string(user), err
}

// socksReadRequest reads a request and returns the command and the raw
// address (ATYP + address + port).
func socksReadRequest(c net.Conn) (byte, []byte, error) {
	h := make([]byte, 4)
	if _, err := io.ReadFull(c, h); err != nil || h[0] != 5 {
		return 0, nil, errors.New("bad request")
	}
	addr, err := socksReadAddr(c, h[3])
	return h[1], addr, err
}

func socksReadAddr(r io.Reader, atyp byte) ([]byte, error) {
	var n int
	switch atyp {
	case 0x01:
		n = 4
	case 0x04:
		n = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return nil, err
		}
		rest := make([]byte, int(l[0])+2)
		if _, err := io.ReadFull(r, rest); err != nil {
			return nil, err
		}
		return append([]byte{0x03, l[0]}, rest...), nil
	default:
		return nil, fmt.Errorf("bad address type %d", atyp)
	}
	b := make([]byte, n+2)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return append([]byte{atyp}, b...), nil
}

func socksReply(c net.Conn, code byte, bnd *net.UDPAddr) error {
	b := []byte{5, code, 0}
	if bnd != nil && bnd.IP.To4() != nil {
		b = append(b, 0x01)
		b = append(b, bnd.IP.To4()...)
	} else if bnd != nil {
		b = append(b, 0x04)
		b = append(b, bnd.IP.To16()...)
	} else {
		b = append(b, 0x01, 0, 0, 0, 0)
	}
	port := 0
	if bnd != nil {
		port = bnd.Port
	}
	b = binary.BigEndian.AppendUint16(b, uint16(port))
	_, err := c.Write(b)
	return err
}

// socksClient authenticates to a SOCKS5 server, sends cmd for addr and
// returns the bound address from the reply.
func socksClient(c net.Conn, user, pass string, cmd byte, addr []byte) ([]byte, error) {
	if _, err := c.Write([]byte{5, 1, 0x02}); err != nil {
		return nil, err
	}
	m := make([]byte, 2)
	if _, err := io.ReadFull(c, m); err != nil || m[1] != 0x02 {
		return nil, errors.New("re-entry refused password auth")
	}
	a := append([]byte{1, byte(len(user))}, user...)
	a = append(append(a, byte(len(pass))), pass...)
	if _, err := c.Write(a); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(c, m); err != nil || m[1] != 0 {
		return nil, errors.New("re-entry rejected credentials")
	}
	if _, err := c.Write(append([]byte{5, cmd, 0}, addr...)); err != nil {
		return nil, err
	}
	h := make([]byte, 4)
	if _, err := io.ReadFull(c, h); err != nil {
		return nil, err
	}
	bnd, err := socksReadAddr(c, h[3])
	if err != nil {
		return nil, err
	}
	if h[1] != 0 {
		return nil, fmt.Errorf("re-entry replied %d", h[1])
	}
	return bnd, nil
}

// socksAddrUDP turns a bound address into the UDP relay endpoint; an
// unspecified address means the server's own host.
func socksAddrUDP(bnd []byte, server string) (*net.UDPAddr, error) {
	if len(bnd) < 3 {
		return nil, errors.New("short address")
	}
	port := int(binary.BigEndian.Uint16(bnd[len(bnd)-2:]))
	var ip net.IP
	switch bnd[0] {
	case 0x01, 0x04:
		ip = net.IP(bnd[1 : len(bnd)-2])
	case 0x03:
		ips, err := net.LookupIP(string(bnd[2 : len(bnd)-2]))
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve relay: %v", err)
		}
		ip = ips[0]
	}
	if ip == nil || ip.IsUnspecified() {
		h, _, _ := net.SplitHostPort(server)
		ip = net.ParseIP(h)
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}
