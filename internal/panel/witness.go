package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Witness arbiter (PR5): a tiny HTTP service that records heartbeats from
// dmit and hk and answers quorum questions for the DNS watchdog. It has no
// database: state lives in memory and is optionally persisted to a JSON
// file, so a restart does not lose the last promotion. It reuses the panel
// binary (polaris witness serve), so the witness host needs no separate
// release artifact and no panel config: it never touches a database or a
// secret key.
//
// Endpoints (all JSON):
//   POST /heartbeat {node}       record a heartbeat, 200 {"ok":true}
//   GET  /verdict?node=dmit     {reachable, lastSeen, secondsSince}
//   POST /promoted {node}        record a promotion, 200 {"ok":true}
//   GET  /primary               {primary, at}
//   GET  /health                200 {"ok":true}
//
// A node counts as reachable when the witness saw its heartbeat within
// witnessHeartbeatTTL. The watchdog only flips DNS when it cannot reach the
// primary, the witness also reports it unreachable, and the watchdog itself
// can reach the witness — two of three agreeing the primary is down.

const witnessHeartbeatTTL = 30 * time.Second

// witnessNodes are the only node names the witness accepts for heartbeats
// and promotions. A typo fails fast (400) instead of silently creating a
// node the watchdog never asks about.
var witnessNodes = map[string]bool{"dmit": true, "hk": true}

// witnessState is the whole witness memory: last heartbeat per node and
// the last recorded promotion. All methods are safe for concurrent use.
type witnessState struct {
	mu         sync.Mutex
	heartbeats map[string]time.Time
	primary    string
	primaryAt  time.Time
	hasPrimary bool
}

func newWitnessState() *witnessState {
	return &witnessState{heartbeats: map[string]time.Time{}}
}

// heartbeat records one heartbeat for node at now.
func (w *witnessState) heartbeat(node string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.heartbeats[node] = now
}

// verdict reports whether node is reachable at now: a heartbeat within
// witnessHeartbeatTTL. ok is false when the node never beat.
func (w *witnessState) verdict(node string, now time.Time) (reachable bool, lastSeen time.Time, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	lastSeen, ok = w.heartbeats[node]
	if !ok {
		return false, time.Time{}, false
	}
	return !now.After(lastSeen.Add(witnessHeartbeatTTL)), lastSeen, true
}

// secondsSince returns whole seconds from lastSeen to now (0 when the node
// never beat, so callers can still print a number).
func witnessSecondsSince(lastSeen time.Time, now time.Time, ok bool) float64 {
	if !ok {
		return 0
	}
	return now.Sub(lastSeen).Seconds()
}

// promote records node as the current primary at now.
func (w *witnessState) promote(node string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.primary = node
	w.primaryAt = now
	w.hasPrimary = true
}

// primaryInfo returns the last recorded promotion ("", zero time, false
// when nothing was ever recorded).
func (w *witnessState) primaryInfo() (string, time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.primary, w.primaryAt, w.hasPrimary
}

// witnessFile is the on-disk shape of witnessState (RFC3339 timestamps, so
// the file stays readable by hand).
type witnessFile struct {
	Heartbeats map[string]time.Time `json:"heartbeats"`
	Primary    string               `json:"primary"`
	PrimaryAt  time.Time            `json:"primaryAt"`
	HasPrimary bool                 `json:"hasPrimary"`
}

func (w *witnessState) snapshot() witnessFile {
	w.mu.Lock()
	defer w.mu.Unlock()
	hb := make(map[string]time.Time, len(w.heartbeats))
	for k, v := range w.heartbeats {
		hb[k] = v
	}
	return witnessFile{Heartbeats: hb, Primary: w.primary, PrimaryAt: w.primaryAt, HasPrimary: w.hasPrimary}
}

func (w *witnessState) restore(f witnessFile) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.heartbeats = f.Heartbeats
	if w.heartbeats == nil {
		w.heartbeats = map[string]time.Time{}
	}
	w.primary = f.Primary
	w.primaryAt = f.PrimaryAt
	w.hasPrimary = f.HasPrimary
}

// save writes the state to path atomically (temp file in the same directory
// + rename). A failure is returned to the caller, which logs it: the
// witness keeps serving from memory either way.
func (w *witnessState) save(path string) error {
	raw, err := json.Marshal(w.snapshot())
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// load reads the state file when it exists. A missing file is an empty
// witness, not an error; a corrupt file is an error, and the caller must
// refuse to serve rather than start empty — the file may hold the last
// promotion, and starting without it could let a demoted primary start
// again (see docs/design/ha-one-click-deployment.md §13.7).
func (w *witnessState) load(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var f witnessFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	w.restore(f)
	return nil
}

// witnessServer is the HTTP front end over a witnessState.
type witnessServer struct {
	state     *witnessState
	stateFile string
	log       *slog.Logger
}

func (s *witnessServer) saveBestEffort() {
	if s.stateFile == "" {
		return
	}
	if err := s.state.save(s.stateFile); err != nil {
		s.log.Warn("witness state save failed", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *witnessServer) heartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var req struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	if !witnessNodes[req.Node] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown node (want dmit or hk)"})
		return
	}
	s.state.heartbeat(req.Node, time.Now())
	s.saveBestEffort()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *witnessServer) verdict(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	node := r.URL.Query().Get("node")
	now := time.Now()
	reachable, lastSeen, ok := s.state.verdict(node, now)
	var last *string
	var since *float64
	if ok {
		v := lastSeen.UTC().Format(time.RFC3339)
		last = &v
		f := witnessSecondsSince(lastSeen, now, ok)
		since = &f
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reachable":    reachable,
		"lastSeen":     last,
		"secondsSince": since,
	})
}

func (s *witnessServer) promoted(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var req struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	if !witnessNodes[req.Node] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown node (want dmit or hk)"})
		return
	}
	s.state.promote(req.Node, time.Now())
	s.saveBestEffort()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *witnessServer) primary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	name, at, ok := s.state.primaryInfo()
	var primary *string
	var atStr *string
	if ok {
		primary = &name
		v := at.UTC().Format(time.RFC3339)
		atStr = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{"primary": primary, "at": atStr})
}

func (s *witnessServer) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *witnessServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/heartbeat", s.heartbeat)
	mux.HandleFunc("/verdict", s.verdict)
	mux.HandleFunc("/promoted", s.promoted)
	mux.HandleFunc("/primary", s.primary)
	mux.HandleFunc("/health", s.health)
	return mux
}

// ServeWitness runs the witness arbiter until SIGINT/SIGTERM. It needs no
// panel config, database or secret key: it only listens and keeps state in
// memory (plus the optional state file).
func ServeWitness(listen, stateFile string) error {
	log := slog.Default()
	st := newWitnessState()
	if stateFile != "" {
		if err := st.load(stateFile); err != nil {
			return fmt.Errorf("witness state load %s: %w", stateFile, err)
		}
	}
	srv := &witnessServer{state: st, stateFile: stateFile, log: log}
	httpSrv := &http.Server{Addr: listen, Handler: srv.handler()}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("witness listen %s: %w", listen, err)
	}
	log.Info("witness serving", "listen", listen)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
