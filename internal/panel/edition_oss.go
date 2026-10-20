//go:build !pro

package panel

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Community build: the paid features (per-user speed limits, the Telegram
// admin menus, backup push) are compiled from the polaris-pro sources with
// -tags pro. Here they are stubs: settings stay editable where harmless and
// the UI shows the features as locked.

const proBuild = false

// userLimits: no limiter in community builds, so no user is limited.
func (s *Server) userLimits(map[string]store.User) []model.UserLimit { return nil }

// pushBackup: the archive stays local.
func (s *Server) pushBackup(string, bool) map[string]string {
	if len(s.pushTargets()) == 0 {
		return map[string]string{}
	}
	return map[string]string{"license": "备份推送是专业版功能（社区版构建不包含），本地备份已保存"}
}

// applyPack: traffic packs are a professional feature.
func (s *Server) applyPack(*gorm.DB, *store.User, *store.TrafficPack, time.Time) (string, error) {
	return "", commandInputError{"流量包是专业版功能（社区版构建不包含）"}
}

// planRates: per-plan node rates are a professional feature; every user
// pays the node's own rate.
func (s *Server) planRates() map[string]map[string]float64 { return nil }

// Online payments (Telegram Stars, EPay, USDT) are a professional feature:
// the community shop sells through redeem codes.
func (s *Server) payments() bool { return false }

func (s *Server) epayNotify(c *gin.Context) { c.String(http.StatusNotFound, "fail") }

func (s *Server) pollUSDT() {}

func (b *tgBot) onPreCheckout(q *tgPreCheckout) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = b.call(ctx, "answerPreCheckoutQuery", map[string]any{"pre_checkout_query_id": q.ID, "ok": false, "error_message": errPayments.Error()}, nil)
}

func (b *tgBot) onStarsPaid(chat int64, p *tgPayment) error {
	b.s.log.Warn("stars payment in a community build", "payload", p.Payload, "charge", p.ChargeID)
	b.send(chat, "已收到付款，但本面板未开启在线收款，请联系管理员（凭证："+esc(p.ChargeID)+"）")
	return nil
}

func (b *tgBot) sendPayment(_ context.Context, chat, msg int64, _ *store.Order, _ string) {
	b.edit(chat, msg, errPayments.Error()+"，可以使用兑换码购买。", []button{{Text: "🎟 兑换码", Data: "v:redeem"}, btnHome})
}

// epayLink: no payment gateway in community builds.
func (s *Server) epayLink(*store.Order, string) string { return "" }
