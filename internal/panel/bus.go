package panel

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// clusterBus carries messages between panel instances that share a
// PostgreSQL database (LISTEN/NOTIFY). Delivery is best effort: a message
// sent while an instance reconnects is lost, so every use has a fallback
// (agents get their revision again with the next heartbeat, a forwarded
// command times out).
type clusterBus struct {
	dsn  string
	db   *gorm.DB
	self string
	log  *slog.Logger

	mu       sync.RWMutex
	handlers map[string][]func(from string, data []byte)
	ready    chan struct{} // closed once listening (tests)
	once     sync.Once
}

const (
	busChannel = "polaris_bus"
	// NOTIFY payloads are limited to 8000 bytes; larger messages travel
	// through the bus_payloads table.
	busInline = 7000
)

type busMsg struct {
	T string          `json:"t"`           // topic
	F string          `json:"f"`           // sending instance
	D json.RawMessage `json:"d,omitempty"` // data
	R string          `json:"r,omitempty"` // bus_payloads row holding the data
}

func newClusterBus(dsn string, db *gorm.DB, self string, log *slog.Logger) *clusterBus {
	return &clusterBus{dsn: dsn, db: db, self: self, log: log, handlers: map[string][]func(string, []byte){}, ready: make(chan struct{})}
}

func (b *clusterBus) subscribe(topic string, fn func(from string, data []byte)) {
	b.mu.Lock()
	b.handlers[topic] = append(b.handlers[topic], fn)
	b.mu.Unlock()
}

// publish sends v (JSON-encoded) to every instance, this one included.
func (b *clusterBus) publish(topic string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m := busMsg{T: topic, F: b.self, D: data}
	if len(data) > busInline {
		row := store.BusPayload{ID: store.NewID(), Data: string(data), CreatedAt: time.Now()}
		if err := b.db.Create(&row).Error; err != nil {
			return err
		}
		b.db.Where("created_at < ?", time.Now().Add(-5*time.Minute)).Delete(&store.BusPayload{})
		m.D, m.R = nil, row.ID
	}
	raw, _ := json.Marshal(m)
	return b.db.Exec("SELECT pg_notify(?, ?)", busChannel, string(raw)).Error
}

// run listens until ctx ends, reconnecting after errors.
func (b *clusterBus) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := b.listen(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("cluster bus disconnected; reconnecting", "err", err)
			sleep(ctx, 2*time.Second)
		}
	}
}

func (b *clusterBus) listen(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, b.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+busChannel); err != nil {
		return err
	}
	b.once.Do(func() { close(b.ready) })
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var m busMsg
		if json.Unmarshal([]byte(n.Payload), &m) != nil {
			continue
		}
		data := []byte(m.D)
		if m.R != "" {
			var row store.BusPayload
			if b.db.First(&row, "id = ?", m.R).Error != nil {
				continue
			}
			data = []byte(row.Data)
		}
		b.mu.RLock()
		hs := b.handlers[m.T]
		b.mu.RUnlock()
		for _, h := range hs {
			go h(m.F, data)
		}
	}
}
