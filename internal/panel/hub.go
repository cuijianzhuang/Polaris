package panel

import (
	"context"
	"fmt"
	"sync"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Hub tracks connected agents and routes messages to their streams.
type Hub struct {
	mu      sync.Mutex
	conns   map[string]*agentConn
	pending map[string]chan *polarisv1.CommandResult
}

type agentConn struct {
	nodeID string
	out    chan *polarisv1.PanelMessage
	done   chan struct{}
}

func newHub() *Hub {
	return &Hub{conns: map[string]*agentConn{}, pending: map[string]chan *polarisv1.CommandResult{}}
}

func (h *Hub) register(nodeID string) *agentConn {
	c := &agentConn{nodeID: nodeID, out: make(chan *polarisv1.PanelMessage, 32), done: make(chan struct{})}
	h.mu.Lock()
	if old, ok := h.conns[nodeID]; ok {
		close(old.done) // a newer stream replaces the old one
	}
	h.conns[nodeID] = c
	h.mu.Unlock()
	return c
}

func (h *Hub) unregister(c *agentConn) {
	h.mu.Lock()
	if cur, ok := h.conns[c.nodeID]; ok && cur == c {
		delete(h.conns, c.nodeID)
		close(c.done)
	}
	h.mu.Unlock()
}

// Online reports whether a node has a live stream.
func (h *Hub) Online(nodeID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.conns[nodeID]
	return ok
}

func (h *Hub) send(nodeID string, m *polarisv1.PanelMessage) bool {
	h.mu.Lock()
	c, ok := h.conns[nodeID]
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case c.out <- m:
		return true
	default:
		return false // slow agent: it will catch up via heartbeat rev comparison
	}
}

// NotifySpec pushes SpecChanged{rev}.
func (h *Hub) NotifySpec(nodeID string, rev int64) {
	h.send(nodeID, &polarisv1.PanelMessage{Msg: &polarisv1.PanelMessage_SpecChanged{SpecChanged: &polarisv1.SpecChanged{Rev: rev}}})
}

// Command sends a structured command and waits for its result.
func (h *Hub) Command(ctx context.Context, nodeID, kind string, args map[string]string, timeout time.Duration) (*polarisv1.CommandResult, error) {
	id := store.NewID()
	ch := make(chan *polarisv1.CommandResult, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()
	ok := h.send(nodeID, &polarisv1.PanelMessage{Msg: &polarisv1.PanelMessage_Command{Command: &polarisv1.Command{Id: id, Kind: kind, Args: args}}})
	if !ok {
		return nil, fmt.Errorf("node is not connected")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("agent did not answer: %w", ctx.Err())
	}
}

func (h *Hub) resolve(r *polarisv1.CommandResult) {
	h.mu.Lock()
	ch, ok := h.pending[r.Id]
	h.mu.Unlock()
	if ok {
		select {
		case ch <- r:
		default:
		}
	}
}
