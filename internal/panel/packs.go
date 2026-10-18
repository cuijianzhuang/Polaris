package panel

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Traffic packs (professional edition) add traffic to an account for the
// rest of its period, without touching the plan or the expiry: the user
// who runs out buys a pack instead of a new plan. A pack may be limited to
// some plans. Applying one is the professional applyPack; this file holds
// the catalogue, the admin API and the shop's pack orders.

// listPacks is GET /traffic-packs.
func (s *Server) listPacks(c *gin.Context) {
	var ps []store.TrafficPack
	s.st.DB.Order("sort, name").Find(&ps)
	for i := range ps {
		ps[i].PlanIDs.V = nonNilStrs(ps[i].PlanIDs.V)
	}
	c.JSON(http.StatusOK, gin.H{"items": ps, "licensed": s.license().Allow(license.FeaturePlanAddons)})
}

type packReq struct {
	Name       string   `json:"name" binding:"required"`
	Traffic    int64    `json:"traffic"`
	PlanIDs    []string `json:"planIds"`
	Sale       bool     `json:"sale"`
	PriceStars int      `json:"priceStars"`
	PriceCNY   float64  `json:"priceCny"`
	PriceUSDT  float64  `json:"priceUsdt"`
	Sort       int      `json:"sort"`
}

func (s *Server) savePack(c *gin.Context, p *store.TrafficPack) {
	if !s.requireFeature(c, license.FeaturePlanAddons) {
		return
	}
	var req packReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	switch {
	case name == "" || len(name) > 64:
		badRequest(c, errors.New("名称不能为空，最长 64 个字符"))
		return
	case req.Traffic < 1<<20 || req.Traffic > 1<<50:
		badRequest(c, errors.New("流量应在 1 MB 到 1 PB 之间"))
		return
	case req.PriceStars < 0 || req.PriceStars > 100000 || req.PriceCNY < 0 || req.PriceCNY > 1e6 || req.PriceUSDT < 0 || req.PriceUSDT > 1e6:
		badRequest(c, errors.New("价格超出范围"))
		return
	}
	plans := dropMissingPlans(s.st.DB, req.PlanIDs)
	p.Name, p.Traffic, p.PlanIDs, p.Sale, p.Sort = name, req.Traffic, store.NewJSON(plans), req.Sale, req.Sort
	p.PriceStars, p.PriceCNY, p.PriceUSDT = req.PriceStars, math.Round(req.PriceCNY*100)/100, math.Round(req.PriceUSDT*100)/100
	if err := s.st.DB.Save(p).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "pack.save", p.Name, fmt.Sprintf("%d bytes", p.Traffic))
	c.JSON(http.StatusOK, p)
}

func dropMissingPlans(db *gorm.DB, ids []string) []string {
	if len(ids) == 0 {
		return []string{}
	}
	var ok []string
	db.Model(&store.Group{}).Where("plan = ? AND id IN ?", true, ids).Pluck("id", &ok)
	return nonNilStrs(ok)
}

// createPack is POST /traffic-packs.
func (s *Server) createPack(c *gin.Context) {
	s.savePack(c, &store.TrafficPack{ID: store.NewID()})
}

// updatePack is PUT /traffic-packs/:id.
func (s *Server) updatePack(c *gin.Context) {
	var p store.TrafficPack
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.savePack(c, &p)
}

// deletePack is DELETE /traffic-packs/:id. Packs already applied stay.
func (s *Server) deletePack(c *gin.Context) {
	var p store.TrafficPack
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Delete(&p)
	s.auditCtx(c, "pack.delete", p.Name, "")
	c.Status(http.StatusNoContent)
}

// addUserPack is POST /users/:id/pack {packId}: applies a pack under the
// user lock, idempotent with an Idempotency-Key.
func (s *Server) addUserPack(c *gin.Context) {
	var req struct {
		PackID string `json:"packId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.userCommand(c, "user.pack", func(tx *gorm.DB, u *store.User, now time.Time) (string, error) {
		var p store.TrafficPack
		if err := tx.First(&p, "id = ?", req.PackID).Error; err != nil {
			return "", commandInputError{"流量包不存在"}
		}
		return s.applyPack(tx, u, &p, now)
	})
}

// packFor reports whether user u may buy pack p, and why not.
func packFor(p *store.TrafficPack, u *store.User) error {
	switch {
	case u == nil:
		return errors.New("请先购买套餐或绑定账号")
	case u.TrafficLimit <= 0:
		return errors.New("你的账号不限流量，不需要流量包")
	case len(p.PlanIDs.V) > 0 && !slices.Contains(p.PlanIDs.V, u.PlanID):
		return errors.New("你当前的套餐不能购买这个流量包")
	}
	return nil
}

// salePacks lists the packs on sale that user u may buy.
func (s *Server) salePacks(u *store.User) []store.TrafficPack {
	if u == nil || !s.license().Allow(license.FeaturePlanAddons) {
		return nil
	}
	var ps, out []store.TrafficPack
	s.st.DB.Where("sale = ?", true).Order("sort, name").Find(&ps)
	for _, p := range ps {
		if packFor(&p, u) == nil {
			out = append(out, p)
		}
	}
	return out
}

// newPackOrder opens a traffic pack order for a bound Telegram user.
func (s *Server) newPackOrder(tgID int64, tgName string, u *store.User, packID, method string) (*store.Order, error) {
	if !s.shopOn() {
		return nil, errShopClosed
	}
	if !s.license().Allow(license.FeaturePlanAddons) {
		return nil, errors.New("流量包暂不可用")
	}
	var p store.TrafficPack
	if s.st.DB.First(&p, "id = ? AND sale = ?", packID, true).Error != nil {
		return nil, errors.New("这个流量包已下架")
	}
	if err := packFor(&p, u); err != nil {
		return nil, err
	}
	amount, currency, ok := s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, method)
	if !ok {
		return nil, errors.New("这个流量包不支持该付款方式")
	}
	now := time.Now()
	var pending int64
	s.st.DB.Model(&store.Order{}).Where("telegram_id = ? AND status = ? AND expires_at > ?", tgID, "pending", now).Count(&pending)
	if pending >= maxPendingOrders {
		return nil, errors.New("未支付的订单太多，请先完成或等待过期")
	}
	if method == payUSDT {
		var err error
		if amount, err = s.uniqueUSDTAmount(p.PriceUSDT, now); err != nil {
			return nil, err
		}
	}
	o := &store.Order{ID: store.NewID(), TelegramID: tgID, TGName: truncate(tgName, 64), UserID: u.ID, Kind: kindPack, PackID: p.ID,
		PlanName: p.Name, Method: method, Amount: amount, Currency: currency, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(s.orderTTL())}
	if err := s.st.DB.Create(o).Error; err != nil {
		return nil, err
	}
	return o, nil
}
