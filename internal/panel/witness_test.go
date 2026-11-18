package panel

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWitnessVerdict covers the heartbeat window: fresh is reachable,
// older than 30 seconds is not, and a node that never beat is unreachable
// with no lastSeen.
func TestWitnessVerdict(t *testing.T) {
	st := newWitnessState()
	now := time.Now()
	if reachable, _, ok := st.verdict("dmit", now); reachable || ok {
		t.Fatalf("never-beat node should be unreachable: %v %v", reachable, ok)
	}
	st.heartbeat("dmit", now)
	if reachable, last, ok := st.verdict("dmit", now.Add(10*time.Second)); !reachable || !ok || !last.Equal(now) {
		t.Fatalf("fresh heartbeat should be reachable: %v %v %v", reachable, ok, last)
	}
	if reachable, _, ok := st.verdict("dmit", now.Add(31*time.Second)); reachable || !ok {
		t.Fatalf("stale heartbeat should be unreachable: %v %v", reachable, ok)
	}
	if s := witnessSecondsSince(now, now.Add(5*time.Second), true); s != 5 {
		t.Fatalf("secondsSince: %v", s)
	}
	if s := witnessSecondsSince(time.Time{}, now, false); s != 0 {
		t.Fatalf("secondsSince without heartbeat: %v", s)
	}
}

// TestWitnessPromote records the current primary and keeps it until the
// next promotion.
func TestWitnessPromote(t *testing.T) {
	st := newWitnessState()
	if name, _, ok := st.primaryInfo(); ok || name != "" {
		t.Fatalf("no promotion recorded yet: %q %v", name, ok)
	}
	now := time.Now()
	st.promote("hk", now)
	name, at, ok := st.primaryInfo()
	if !ok || name != "hk" || !at.Equal(now) {
		t.Fatalf("primary: %q %v %v", name, ok, at)
	}
	st.promote("dmit", now.Add(time.Minute))
	if name, _, _ := st.primaryInfo(); name != "dmit" {
		t.Fatalf("promotion should move: %q", name)
	}
}

// TestWitnessPersistence saves the state to disk and loads it back: a
// restart keeps both the heartbeats and the last promotion.
func TestWitnessPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witness.json")
	st := newWitnessState()
	now := time.Now().Truncate(time.Second)
	st.heartbeat("dmit", now)
	st.promote("hk", now)
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind")
	}
	restored := newWitnessState()
	if err := restored.load(path); err != nil {
		t.Fatal(err)
	}
	if reachable, _, ok := restored.verdict("dmit", now.Add(5*time.Second)); !reachable || !ok {
		t.Fatalf("restored heartbeat should be reachable: %v %v", reachable, ok)
	}
	if name, _, ok := restored.primaryInfo(); !ok || name != "hk" {
		t.Fatalf("restored primary: %q %v", name, ok)
	}
	// A missing file is an empty witness, not an error; a corrupt file is.
	empty := newWitnessState()
	if err := empty.load(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatalf("missing state file: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := empty.load(bad); err == nil {
		t.Fatal("corrupt state file should error")
	}
}

// TestWitnessHTTP runs the whole heartbeat -> verdict -> promoted ->
// primary flow over real HTTP, including the 400s for unknown nodes.
// TestWitnessCorruptStateRefusesToServe: a corrupt state file must fail
// startup instead of starting empty — the file may hold the last
// promotion, and losing it could let a demoted primary start again.
func TestWitnessCorruptStateRefusesToServe(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- ServeWitness("127.0.0.1:0", bad) }()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("corrupt state file should fail witness startup")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeWitness with a corrupt state file did not return")
	}
}

func TestWitnessHTTP(t *testing.T) {
	srv := &witnessServer{state: newWitnessState(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	get := func(path string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	if code, _ := get("/health"); code != 200 {
		t.Fatalf("health: %d", code)
	}
	if code, out := get("/primary"); code != 200 || out["primary"] != nil {
		t.Fatalf("primary before any promotion: %d %v", code, out)
	}
	if code, out := get("/verdict?node=dmit"); code != 200 || out["reachable"] != false || out["lastSeen"] != nil {
		t.Fatalf("verdict before any heartbeat: %d %v", code, out)
	}
	if code, _ := post("/heartbeat", `{"node":"dmit"}`); code != 200 {
		t.Fatalf("heartbeat dmit: %d", code)
	}
	if code, out := get("/verdict?node=dmit"); code != 200 || out["reachable"] != true || out["lastSeen"] == nil {
		t.Fatalf("verdict after heartbeat: %d %v", code, out)
	}
	if code, out := get("/verdict?node=hk"); code != 200 || out["reachable"] != false {
		t.Fatalf("verdict for the other node: %d %v", code, out)
	}
	if code, _ := post("/heartbeat", `{"node":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("heartbeat unknown node: %d, want 400", code)
	}
	if code, _ := post("/heartbeat", `{broken`); code != http.StatusBadRequest {
		t.Fatalf("heartbeat bad json: %d, want 400", code)
	}
	if code, _ := post("/promoted", `{"node":"hk"}`); code != 200 {
		t.Fatalf("promoted hk: %d", code)
	}
	if code, out := get("/primary"); code != 200 || out["primary"] != "hk" || out["at"] == nil {
		t.Fatalf("primary after promotion: %d %v", code, out)
	}
	if code, _ := post("/promoted", `{"node":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("promoted unknown node: %d, want 400", code)
	}
	if code, _ := post("/verdict?node=dmit", `{}`); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST verdict: %d, want 405", code)
	}
}
