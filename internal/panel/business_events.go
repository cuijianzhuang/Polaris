package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

// queueBusinessEvent is a transactional outbox using the existing delivery
// table. Callers MUST pass the transaction that changes the business record.
// Recipients, event id, timestamp and redacted payload are frozen at commit.
// No network I/O or reads through s.st.DB are allowed inside this function.
func (s *Server) queueBusinessEvent(tx *gorm.DB, e Event) error {
	typ := eventType(e)
	if !strings.HasPrefix(typ, "user.") && !strings.HasPrefix(typ, "order.") && !strings.HasPrefix(typ, "ticket.") {
		return errors.New("unsupported transactional business event")
	}
	var hooks []store.Webhook
	if err := tx.Where("enabled = ?", true).Find(&hooks).Error; err != nil {
		return err
	}
	payload := s.eventPayloadDB(tx, e, typ)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, hook := range hooks {
		if !matchEvent(hook.Events.V, typ) {
			continue
		}
		id := sha256.Sum256([]byte(payload["id"].(string) + "\x00" + hook.ID))
		d := store.WebhookDelivery{ID: hex.EncodeToString(id[:16]), WebhookID: hook.ID,
			EventID: payload["id"].(string), Type: typ, Payload: string(body), Status: "pending", NextAt: now, CreatedAt: now}
		if err := tx.Create(&d).Error; err != nil {
			return err // abort the business mutation too
		}
	}
	return nil
}

func (s *Server) queueUserEvent(tx *gorm.DB, typ string, u *store.User, extra map[string]any) error {
	data := map[string]any{"user": userBrief(u)}
	for k, v := range extra {
		data[k] = v
	}
	return s.queueBusinessEvent(tx, Event{Kind: typ, UserID: u.ID, Data: data})
}

// Best-effort notifications retain their existing behavior; the webhook for
// this committed event is already queued and must never be emitted twice.
func (s *Server) notifyCommitted(e Event) {
	for _, sink := range s.sinks {
		if sink == s.hookEvents {
			continue
		}
		select {
		case sink <- e:
		default:
		}
	}
	s.wakeBusinessEvents()
}

func (s *Server) wakeBusinessEvents() {
	select {
	case s.businessEvents <- struct{}{}:
	default: // cron also drains committed tasks after a crash or a full wakeup channel
	}
}
