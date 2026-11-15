package panel

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestPruneNodeCachesPreservesLiveGraceAndReturnedMaps(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.DB.Create(&store.Node{ID: "live", Name: "disconnected", Status: "offline"}).Error; err != nil {
		t.Fatal(err)
	}
	graceUntil := time.Now().Add(nodeUsersGrace)
	live := nodeUsersEntry{set: map[string]bool{"current": true, "late-traffic": true}, own: map[string]bool{"current": true}, graceUntil: graceUntil}
	oldSet := map[string]bool{"deleted-user": true}
	s.nodeUserSets.m = map[string]nodeUsersEntry{}
	s.nodeUserSets.storeLocked("live", live)
	s.nodeUserSets.storeLocked("deleted", nodeUsersEntry{set: oldSet})
	old, recent := time.Now().Add(-2*nodeThrottleRetention), time.Now()
	for _, cache := range []map[string]time.Time{s.specAt, s.presAt, s.shareAt} {
		cache["old"] = old
		cache["recent"] = recent
	}
	s.shareAt["limited:deleted"] = old
	s.shareAt["progress:deleted"] = old

	s.pruneNodeCaches()
	if _, ok := s.nodeUserSets.m["deleted"]; ok {
		t.Fatal("deleted node cache retained")
	}
	entry, ok := s.nodeUserSets.m["live"]
	if !ok || !entry.set["late-traffic"] || !entry.set["current"] || !entry.graceUntil.Equal(graceUntil) {
		t.Fatalf("disconnected live node lost grace: %+v", entry)
	}
	if !oldSet["deleted-user"] {
		t.Fatal("sweep mutated a map still held by a reader")
	}
	for _, cache := range []map[string]time.Time{s.specAt, s.presAt, s.shareAt} {
		if len(cache) != 1 || !cache["recent"].Equal(recent) {
			t.Fatalf("throttle sweep changed active state or retained idle state: %+v", cache)
		}
	}
}

func TestPruneNodeCachesBatchesAndKeepsAllOnReadFailure(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.DB.Create(&store.Node{ID: "live", Name: "live"}).Error; err != nil {
		t.Fatal(err)
	}
	s.nodeUserSets.m = map[string]nodeUsersEntry{}
	s.nodeUserSets.storeLocked("live", nodeUsersEntry{})
	for i := 0; i < 500; i++ {
		s.nodeUserSets.storeLocked(fmt.Sprintf("deleted-%d", i), nodeUsersEntry{})
	}
	queries := 0
	const callback = "test:node-cache-read-failure"
	if err := s.st.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			queries++
			if queries == 2 {
				tx.AddError(errors.New("temporary database failure"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Query().Remove(callback) })
	s.pruneNodeCaches()
	s.st.DB.Callback().Query().Remove(callback)
	if queries != 2 || len(s.nodeUserSets.m) != 501 {
		t.Fatalf("failed lookup partially evicted entries: queries=%d entries=%d", queries, len(s.nodeUserSets.m))
	}
	s.pruneNodeCaches()
	if len(s.nodeUserSets.m) != 1 {
		t.Fatalf("deleted entries remain after recovery: %d", len(s.nodeUserSets.m))
	}
	if _, ok := s.nodeUserSets.m["live"]; !ok {
		t.Fatal("live node evicted")
	}
}

func TestPruneNodeCachesDoesNotEvictRefreshedGeneration(t *testing.T) {
	s := newTestServer(t)
	s.nodeUserSets.m = map[string]nodeUsersEntry{}
	s.nodeUserSets.storeLocked("deleted", nodeUsersEntry{set: map[string]bool{"before": true}})
	const callback = "test:node-cache-concurrent-refresh"
	if err := s.st.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			// Models a handler that had already read its node before deletion
			// and writes the cache after the sweep's existence query.
			s.nodeUserSets.mu.Lock()
			s.nodeUserSets.storeLocked("deleted", nodeUsersEntry{set: map[string]bool{"after": true}})
			s.nodeUserSets.mu.Unlock()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Query().Remove(callback) })
	s.pruneNodeCaches()
	s.st.DB.Callback().Query().Remove(callback)
	if entry, ok := s.nodeUserSets.m["deleted"]; !ok || !entry.set["after"] {
		t.Fatal("sweep removed a concurrently refreshed cache entry")
	}
	s.pruneNodeCaches()
	if len(s.nodeUserSets.m) != 0 {
		t.Fatal("next sweep did not reclaim late handler's stale entry")
	}
}

func TestNodeUsersCacheConcurrentSweep(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.DB.Create(&store.Node{ID: "live", Name: "live"}).Error; err != nil {
		t.Fatal(err)
	}
	s.nodeUserSets.m = map[string]nodeUsersEntry{}
	s.nodeUserSets.storeLocked("live", nodeUsersEntry{set: map[string]bool{"u": true}})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if !s.nodeUsers("live")["u"] {
				t.Error("concurrent sweep lost live users")
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			s.pruneNodeCaches()
		}
	}()
	wg.Wait()
}

func TestNodeUsersCacheHitAdvancesGeneration(t *testing.T) {
	s := newTestServer(t)
	if err := s.st.DB.Create(&store.Node{ID: "live", Name: "live"}).Error; err != nil {
		t.Fatal(err)
	}
	s.nodeUserSets.m = map[string]nodeUsersEntry{}
	s.nodeUserSets.storeLocked("live", nodeUsersEntry{set: map[string]bool{"u": true}})
	before := s.nodeUserSets.m["live"].generation
	if !s.nodeUsers("live")["u"] || s.nodeUserSets.m["live"].generation <= before {
		t.Fatal("cache hit did not protect entry from concurrent sweep")
	}
}
