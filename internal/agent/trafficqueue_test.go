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
	a.loadQueue()
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
