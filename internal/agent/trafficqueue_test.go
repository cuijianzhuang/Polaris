package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/api/gen/polaris/v1/polarisv1connect"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// trafficCore reports a fixed delta on every collection.
type trafficCore struct{ fakeCore }

func (*trafficCore) CollectTraffic(context.Context) ([]model.TrafficDelta, error) {
	return []model.TrafficDelta{{UserID: "u", Up: 1, Down: 1}}, nil
}

func newQueueAgent(t *testing.T, dir string) *Agent {
	a := &Agent{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := os.MkdirAll(a.idDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.loadQueue(); err != nil {
		t.Fatal(err)
	}
	return a
}

func collectOnce(a *Agent) {
	a.collectFrom(context.Background(), &managed{adapter: &trafficCore{}})
}

// Sequence numbers keep counting across restarts within one epoch.
func TestTrafficQueueSeqPersists(t *testing.T) {
	dir := t.TempDir()
	a := newQueueAgent(t, dir)
	collectOnce(a)
	collectOnce(a)
	b := newQueueAgent(t, dir)
	collectOnce(b)
	if len(b.queue) != 3 || b.epoch != a.epoch {
		t.Fatalf("queue %d epoch %q vs %q", len(b.queue), b.epoch, a.epoch)
	}
	for i, x := range b.queue {
		if x.Seq != uint64(i+1) || x.Epoch != a.epoch {
			t.Fatalf("batch %d: seq %d epoch %q", i, x.Seq, x.Epoch)
		}
	}
}

// A queue written by an older agent (a bare array) still loads; its
// batches keep no sequence number.
func TestTrafficQueueLegacyFormat(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{cfg: Config{DataDir: dir}}
	_ = os.MkdirAll(a.idDir(), 0o700)
	old, _ := json.Marshal([]*polarisv1.TrafficBatch{{BatchId: "old", Users: []*polarisv1.UserTraffic{{UserId: "u", Up: 5}}}})
	if err := os.WriteFile(filepath.Join(a.idDir(), "traffic-queue.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	b := newQueueAgent(t, dir)
	collectOnce(b)
	if len(b.queue) != 2 || b.queue[0].BatchId != "old" || b.queue[0].Seq != 0 || b.queue[1].Seq != 1 || b.epoch == "" {
		t.Fatalf("queue: %+v epoch %q", b.queue, b.epoch)
	}
}

type slowClient struct {
	polarisv1connect.AgentServiceClient
	entered chan struct{}
	release chan struct{}
}

func (c *slowClient) ReportTraffic(_ context.Context, req *connect.Request[polarisv1.ReportTrafficRequest]) (*connect.Response[polarisv1.ReportTrafficResponse], error) {
	close(c.entered)
	<-c.release
	resp := &polarisv1.ReportTrafficResponse{}
	for _, b := range req.Msg.Batches {
		resp.AcceptedBatchIds = append(resp.AcceptedBatchIds, b.BatchId)
	}
	return connect.NewResponse(resp), nil
}

// Collection must not wait for a slow upload, and batches queued during the
// upload stay queued.
func TestTrafficFlushDoesNotBlockCollect(t *testing.T) {
	a := newQueueAgent(t, t.TempDir())
	collectOnce(a)
	c := &slowClient{entered: make(chan struct{}), release: make(chan struct{})}
	a.client = c
	done := make(chan struct{})
	go func() { a.flushTraffic(context.Background()); close(done) }()
	<-c.entered
	collected := make(chan struct{})
	go func() { collectOnce(a); close(collected) }()
	select {
	case <-collected:
	case <-time.After(2 * time.Second):
		t.Fatal("collection blocked by the upload")
	}
	c.entered = make(chan struct{}) // the second round uploads the new batch
	close(c.release)
	<-done
	if len(a.queue) != 0 {
		t.Fatalf("left in queue: %d", len(a.queue))
	}
}

// Broken storage must stop uploads before an unpersisted sequence is accepted.
// Once storage recovers, the same batch is written, uploaded and recoverable.
func TestTrafficQueueWriteFailureAndRecovery(t *testing.T) {
	dir := t.TempDir()
	a := newQueueAgent(t, dir)
	iddir := a.idDir()
	held := iddir + "-held"
	if e := os.Rename(iddir, held); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(iddir, []byte("not a directory"), 0600); e != nil {
		t.Fatal(e)
	}
	restore := func() {
		os.Remove(iddir)
		if e := os.Rename(held, iddir); e != nil {
			t.Fatal(e)
		}
	}
	collectOnce(a)
	if !a.queueDirty || len(a.queue) != 1 {
		t.Fatal("write failure did not keep dirty batch")
	}
	// No client is installed: a call would panic if the dirty batch were sent.
	a.flushTraffic(context.Background())
	before := a.nextSeq
	if a.retryQueue() {
		t.Fatal("broken storage reported as healthy")
	}
	restore()
	if !a.retryQueue() || a.queueDirty {
		t.Fatal("queue did not recover")
	}
	b := newQueueAgent(t, dir)
	if len(b.queue) != 1 || b.nextSeq != before || b.queue[0].Seq != 1 {
		t.Fatalf("recovered state: %+v", b.queue)
	}
	c := &slowClient{entered: make(chan struct{}), release: make(chan struct{})}
	close(c.release)
	a.client = c
	a.flushTraffic(context.Background())
	b = newQueueAgent(t, dir)
	if len(b.queue) != 0 || b.nextSeq != before {
		t.Fatal("acknowledgement lost durable cursor")
	}
	collectOnce(b)
	if b.queue[0].Seq != 2 {
		t.Fatal("sequence regressed after acknowledgement")
	}
}

func TestTrafficQueueCorruptionRefused(t *testing.T) {
	for _, data := range []string{`broken`, `{}`, `{"epoch":"e","nextSeq":1,"batches":[{"epoch":"e","seq":1}]}`} {
		a := newQueueAgent(t, t.TempDir())
		if e := os.WriteFile(a.queuePath(), []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
		if e := a.loadQueue(); e == nil {
			t.Fatal("corrupt queue silently reset", data)
		}
	}
}

type ackHookClient struct {
	polarisv1connect.AgentServiceClient
	hook func()
}

func (c *ackHookClient) ReportTraffic(_ context.Context, r *connect.Request[polarisv1.ReportTrafficRequest]) (*connect.Response[polarisv1.ReportTrafficResponse], error) {
	c.hook()
	out := &polarisv1.ReportTrafficResponse{}
	for _, b := range r.Msg.Batches {
		out.AcceptedBatchIds = append(out.AcceptedBatchIds, b.BatchId)
	}
	return connect.NewResponse(out), nil
}
func TestTrafficQueueAckWriteFailureKeepsSequence(t *testing.T) {
	dir := t.TempDir()
	a := newQueueAgent(t, dir)
	collectOnce(a)
	iddir := a.idDir()
	held := iddir + "-held"
	a.client = &ackHookClient{hook: func() {
		if e := os.Rename(iddir, held); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(iddir, []byte("blocked"), 0600); e != nil {
			t.Fatal(e)
		}
	}}
	a.flushTraffic(context.Background())
	if !a.queueDirty {
		t.Fatal("ack write failure was ignored")
	}
	os.Remove(iddir)
	if e := os.Rename(held, iddir); e != nil {
		t.Fatal(e)
	}
	// A crash before the prune was persisted replays the acknowledged batch;
	// it never reuses that batch's sequence for new traffic.
	b := newQueueAgent(t, dir)
	collectOnce(b)
	if len(b.queue) != 2 || b.queue[0].Seq != 1 || b.queue[1].Seq != 2 || b.nextSeq != 3 {
		t.Fatal("ack rollback reused a sequence", b.queue)
	}
}

func TestTrafficQueueHealthSnapshot(t *testing.T) {
	a := newQueueAgent(t, t.TempDir())
	collectOnce(a)
	a.queueMu.Lock()
	a.queueDirty = true
	a.queueDropped = 2
	a.queueMu.Unlock()
	st := a.trafficQueueStatus()
	if st.PendingBatches != 1 || st.OldestUnackedAt <= 0 || !st.PersistenceFailed || st.DroppedBatches != 2 {
		t.Fatal("queue health missing", st)
	}
	a.queueMu.Lock()
	a.queue = nil
	a.queueDirty = false
	a.queueMu.Unlock()
	st = a.trafficQueueStatus()
	if st.PendingBatches != 0 || st.OldestUnackedAt != 0 || st.PersistenceFailed {
		t.Fatal("stale queue health", st)
	}
}
