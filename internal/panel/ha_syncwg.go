package panel

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// WireGuard peer reconciliation (PR2).
//
// The panel server never executes system commands, so this lives in the
// maintenance CLI (polaris ha sync-wg), run by a systemd timer on the
// primary. It reads the wanted peers from the ha_peers table, compares them
// with the live peers from `wg show wg0 dump`, applies the difference with
// `wg set`, and rewrites wg0.conf so a reboot keeps the same peers.
//
// The binaries and the config path are package variables so tests can point
// them at fixtures; no other code in this package calls os/exec.

var (
	// haWGConfPath is the WireGuard config rewritten after every sync.
	haWGConfPath = "/etc/wireguard/wg0.conf"
	// haWGBinary is the wg binary.
	haWGBinary = "wg"
	// haSystemctlBinary starts the tunnel when the interface is down.
	haSystemctlBinary = "systemctl"
)

// wgPeerState is one live peer as reported by `wg show wg0 dump`.
type wgPeerState struct {
	Pubkey     string
	Endpoint   string
	AllowedIPs string
	// Handshake is the latest-handshake column as unix seconds (0 = the
	// peer has never completed a handshake).
	Handshake int64
}

// parseWGDump parses `wg show <iface> dump`. The first line is the
// interface (public key, private key, listening port, fwmark); the rest are
// peers: public-key, preshared-key, endpoint, allowed-ips, latest-handshake,
// transfer-rx, transfer-tx, persistent-keepalive. Lines that do not parse
// are skipped, never fatal: a partial view is safer than no sync.
func parseWGDump(out string) map[string]wgPeerState {
	have := make(map[string]wgPeerState)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		st := wgPeerState{Pubkey: fields[0], Endpoint: fields[2], AllowedIPs: fields[3]}
		if len(fields) >= 5 {
			st.Handshake, _ = strconv.ParseInt(fields[4], 10, 64)
		}
		have[fields[0]] = st
	}
	return have
}

// peerLastSeenUpdates returns the ha_peers rows (by WG IP) whose last_seen
// should advance to the peer's latest handshake. A handshake of 0 (never)
// never updates, and the stored value never moves backwards, so the HA
// page's "last seen" is the newest handshake this node has observed.
func peerLastSeenUpdates(peers []store.HAPeer, have map[string]wgPeerState) map[string]time.Time {
	out := map[string]time.Time{}
	for _, p := range peers {
		live, ok := have[p.Pubkey]
		if !ok || live.Handshake <= 0 {
			continue
		}
		hs := time.Unix(live.Handshake, 0).UTC()
		if p.LastSeen == nil || hs.After(*p.LastSeen) {
			out[p.WGIP] = hs
		}
	}
	return out
}

// diffWGPeers compares the wanted peers (ha_peers rows) with the live
// state. A peer is added when its pubkey is missing, its endpoint differs,
// or its allowed-ips do not contain its WireGuard address; a live pubkey
// with no wanted row is removed. Adding the same peer again is idempotent
// on the wg side, so re-adding a drifted peer is the simplest repair.
func diffWGPeers(want []store.HAPeer, have map[string]wgPeerState) (add []store.HAPeer, remove []string) {
	for _, p := range want {
		live, ok := have[p.Pubkey]
		if !ok {
			add = append(add, p)
			continue
		}
		wantEndpoint := ""
		if p.Endpoint != "" {
			wantEndpoint = p.Endpoint + ":" + fmt.Sprint(haWGPort)
		}
		if wantEndpoint != "" && live.Endpoint != "" && live.Endpoint != wantEndpoint {
			add = append(add, p)
			continue
		}
		if !strings.Contains(live.AllowedIPs, p.WGIP+"/32") {
			add = append(add, p)
		}
	}
	for pubkey := range have {
		found := false
		for _, p := range want {
			if p.Pubkey == pubkey {
				found = true
				break
			}
		}
		if !found {
			remove = append(remove, pubkey)
		}
	}
	sort.Strings(remove)
	return add, remove
}

// haWGUp makes sure the wg0 interface is up, starting it with
// wg-quick when it is not.
func haWGUp() error {
	if err := exec.Command(haWGBinary, "show", "wg0").Run(); err == nil {
		return nil
	}
	if err := exec.Command(haSystemctlBinary, "start", "wg-quick@wg0").Run(); err != nil {
		return fmt.Errorf("start wg-quick@wg0: %w", err)
	}
	if err := exec.Command(haWGBinary, "show", "wg0").Run(); err != nil {
		return fmt.Errorf("wg0 still down after wg-quick start: %w", err)
	}
	return nil
}

// rewriteWGConf rewrites wg0.conf: the [Interface] section (and anything
// before the first [Peer]) is kept verbatim, all old [Peer] sections are
// dropped, and the wanted peers are appended. The write is atomic (temp
// file in the same directory + rename) and the file stays 0600.
func rewriteWGConf(path string, peers []store.HAPeer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	end := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[Peer]") {
			end = i
			break
		}
	}
	head := lines[:end]
	// Trim trailing blank lines of the kept head, then append the peers.
	for len(head) > 0 && strings.TrimSpace(head[len(head)-1]) == "" {
		head = head[:len(head)-1]
	}
	var b strings.Builder
	for _, line := range head {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, p := range peers {
		b.WriteString("\n[Peer]\n")
		b.WriteString("PublicKey = " + p.Pubkey + "\n")
		b.WriteString("AllowedIPs = " + p.WGIP + "/32\n")
		if p.Endpoint != "" {
			b.WriteString("Endpoint = " + p.Endpoint + ":" + fmt.Sprint(haWGPort) + "\n")
		}
		b.WriteString("PersistentKeepalive = 25\n")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// SyncWG reconciles the local WireGuard peers with the ha_peers table and
// returns a one-line summary ("wireguard peers: +1 -0 (1 total)"). It is
// only reachable from the maintenance CLI, never from the server process.
func SyncWG(cfg *Config) (string, error) {
	st, err := openStore(cfg)
	if err != nil {
		return "", err
	}
	defer st.Close()
	var peers []store.HAPeer
	if err := st.DB.Order("wg_ip").Find(&peers).Error; err != nil {
		return "", err
	}
	if err := haWGUp(); err != nil {
		return "", err
	}
	out, err := exec.Command(haWGBinary, "show", "wg0", "dump").Output()
	if err != nil {
		return "", fmt.Errorf("wg show wg0 dump: %w", err)
	}
	have := parseWGDump(string(out))
	// Refresh last_seen from live handshakes: the column is otherwise only
	// ever set at join time, so the HA page would show the join date
	// forever even while the tunnel (and replication over it) is healthy.
	for wgIP, hs := range peerLastSeenUpdates(peers, have) {
		if err := st.DB.Model(&store.HAPeer{}).Where("wg_ip = ?", wgIP).Update("last_seen", hs).Error; err != nil {
			return "", fmt.Errorf("update peer %s last_seen: %w", wgIP, err)
		}
	}
	add, remove := diffWGPeers(peers, have)
	for _, p := range add {
		args := []string{"set", "wg0", "peer", p.Pubkey, "allowed-ips", p.WGIP + "/32"}
		if p.Endpoint != "" {
			args = append(args, "endpoint", p.Endpoint+":"+fmt.Sprint(haWGPort))
		}
		args = append(args, "persistent-keepalive", "25")
		if err := exec.Command(haWGBinary, args...).Run(); err != nil {
			return "", fmt.Errorf("wg set peer %s: %w", p.Pubkey, err)
		}
	}
	for _, pubkey := range remove {
		if err := exec.Command(haWGBinary, "set", "wg0", "peer", pubkey, "remove").Run(); err != nil {
			return "", fmt.Errorf("wg remove peer %s: %w", pubkey, err)
		}
	}
	if err := rewriteWGConf(haWGConfPath, peers); err != nil {
		return "", fmt.Errorf("rewrite %s: %w", haWGConfPath, err)
	}
	return fmt.Sprintf("wireguard peers: +%d -%d (%d total)", len(add), len(remove), len(peers)), nil
}
