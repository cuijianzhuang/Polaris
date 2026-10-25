package panel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Bot shop: /buy (or 🛒 in the menu) lists the plans on sale; a plan offers
// the configured ways to pay. Callbacks: s:p:<plan> plan card,
// s:m:<plan>:<method>[:<type>] new order, s:q:<order> payment check,
// s:r:<order>[:<type>] pay an open order again, s:c / s:x:<order> cancel
// (ask, then do). v:unpaid lists the open orders.

type tgPreCheckout struct {
	ID          string `json:"id"`
	From        tgUser `json:"from"`
	Currency    string `json:"currency"`
	TotalAmount int64  `json:"total_amount"`
	Payload     string `json:"invoice_payload"`
}

type tgPayment struct {
	Currency    string `json:"currency"`
	TotalAmount int64  `json:"total_amount"`
	Payload     string `json:"invoice_payload"`
	ChargeID    string `json:"telegram_payment_charge_id"`
}

var epayTypeName = map[string]string{"alipay": "支付宝", "wxpay": "微信支付", "qqpay": "QQ 钱包", "usdt": "USDT", "bank": "网银"}

func (b *tgBot) username() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.me
}

// planPrices is a plan's prices for the shop list ("¥15 · 150 ⭐ · 2.5 USDT").
func (s *Server) planPrices(p *store.Group) string {
	return s.productPrices(p.PriceStars, p.PriceCNY, p.PriceUSDT)
}

// productPrices lists a product's prices in the configured ways to pay.
func (s *Server) productPrices(stars int, cny, usdt float64) string {
	var parts []string
	if _, _, ok := s.priceFor(stars, cny, usdt, payEPay); ok {
		parts = append(parts, "¥"+strconv.FormatFloat(cny, 'f', -1, 64))
	}
	if _, _, ok := s.priceFor(stars, cny, usdt, payStars); ok {
		parts = append(parts, strconv.Itoa(stars)+" ⭐")
	}
	if _, _, ok := s.priceFor(stars, cny, usdt, payUSDT); ok {
		parts = append(parts, strconv.FormatFloat(usdt, 'f', -1, 64)+" USDT")
	}
	return strings.Join(parts, " · ")
}

// tgPlanLine summarises a plan: traffic, days, devices, nodes.
func (s *Server) tgPlanLine(p *store.Group) string {
	parts := []string{"不限流量"}
	if p.TrafficLimit > 0 {
		parts[0] = sub.HumanBytes(p.TrafficLimit)
	}
	if p.Days > 0 {
		parts = append(parts, fmt.Sprintf("%d 天", p.Days))
	} else {
		parts = append(parts, "不限期")
	}
	if p.DeviceLimit > 0 {
		parts = append(parts, fmt.Sprintf("%d 台设备", p.DeviceLimit))
	}
	var n int64
	s.st.DB.Raw(`SELECT COUNT(*) FROM nodes WHERE (level > 0 AND level <= ?) OR id IN (SELECT node_id FROM group_nodes WHERE group_id = ?)`, p.Level, p.ID).Scan(&n)
	if n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个节点", n))
	}
	return strings.Join(parts, " · ")
}

func (b *tgBot) shopView(id tgIdentity) (string, [][]button) {
	if !b.s.shopOn() {
		return "🛒 <b>购买套餐</b>\n" + tgRule + "\n商店暂未开放。", [][]button{backRow("")}
	}
	var sb strings.Builder
	sb.WriteString("🛒 <b>购买套餐</b>\n" + tgRule + "\n")
	var rows [][]button
	for _, p := range b.s.salePlans() {
		prices := b.s.planPrices(&p)
		if prices == "" {
			if b.s.payments() {
				continue // no way to pay for it here
			}
			prices = orDefault(p.Price, "暂未开放付款方式")
			if b.s.codesOn() {
				prices = orDefault(p.Price, "兑换码购买")
			}
		}
		fmt.Fprintf(&sb, "\n<b>%s</b>  %s\n%s\n", esc(p.Name), esc(prices), esc(b.s.tgPlanLine(&p)))
		label := p.Name
		if id.user != nil && id.user.PlanID == p.ID {
			label = "🔄 续费 " + p.Name
		}
		rows = append(rows, []button{{Text: label, Data: "s:p:" + p.ID}})
	}
	if len(rows) == 0 {
		sb.WriteString("\n暂时没有在售的套餐。")
	}
	if id.user == nil && !b.s.shopAutoCreate() {
		sb.WriteString("\n\n购买前请先绑定账号。")
	}
	if len(b.s.salePacks(id.user)) > 0 {
		rows = append(rows, []button{{Text: "📦 加购流量包", Data: "v:packs"}})
	}
	if n := b.s.unpaidCount(id.chat); n > 0 {
		rows = append(rows, []button{{Text: fmt.Sprintf("⏳ 未付款订单（%d）", n), Data: "v:unpaid"}})
	}
	codeRow := []button{{Text: "🏷 优惠码", Data: "v:coupon"}}
	if b.s.codesOn() {
		codeRow = append(codeRow, button{Text: "🎟 兑换码", Data: "v:redeem"})
	}
	rows = append(rows, codeRow, []button{{Text: "📋 我的订单", Data: "v:orders"}})
	if b.s.shopTerms() != "" {
		rows = append(rows, []button{{Text: "📄 购买条款", Data: "v:terms"}, {Text: "💬 售后", Data: "v:paysupport"}})
	}
	rows = append(rows, []button{btnHome})
	return sb.String(), rows
}

func (s *Server) shopTerms() string { return strings.TrimSpace(s.st.GetSetting("shop_terms", "")) }

func (b *tgBot) termsView() string {
	t := b.s.shopTerms()
	if t == "" {
		t = "购买的套餐付款后立即开通；续费同一套餐顺延到期时间，换套餐从开通当天计时并清零已用流量。"
	}
	return "📄 <b>购买条款</b>\n" + tgRule + "\n" + esc(t)
}

func (b *tgBot) supportView() string {
	sup := strings.TrimSpace(b.s.st.GetSetting("shop_support", ""))
	if sup == "" {
		sup = "请联系管理员。"
	}
	return "💬 <b>支付与售后</b>\n" + tgRule + "\n" + esc(sup) + "\n\n联系时请提供订单号（在「我的订单」里查看）。不要发送密码等敏感信息。"
}

// orderAmount is how an order's price reads in the bot.
func orderAmount(o *store.Order) string {
	switch o.Method {
	case payCode:
		return "兑换码"
	case payCoupon:
		return "优惠券免费"
	case payStars:
		return o.Amount + " ⭐"
	case payEPay:
		return "¥" + o.Amount
	}
	return o.Amount + " " + o.Currency
}

// ordersView lists the chat's recent orders.
func (b *tgBot) ordersView(chat int64) (string, [][]button) {
	var os []store.Order
	b.s.st.DB.Where("telegram_id = ?", chat).Order("created_at desc").Limit(10).Find(&os)
	var sb strings.Builder
	sb.WriteString("📋 <b>我的订单</b>\n" + tgRule + "\n")
	if len(os) == 0 {
		sb.WriteString("还没有订单。")
	}
	for _, o := range os {
		fmt.Fprintf(&sb, "\n<code>%s</code> %s\n%s · %s · %s\n", o.ID, orderStatusText[o.Status], esc(o.PlanName), esc(orderAmount(&o)), o.CreatedAt.Format("01-02 15:04"))
	}
	var rows [][]button
	if n := b.s.unpaidCount(chat); n > 0 {
		rows = append(rows, []button{{Text: fmt.Sprintf("⏳ 未付款订单（%d）", n), Data: "v:unpaid"}})
	}
	rows = append(rows, []button{{Text: "« 返回", Data: "v:shop"}, btnHome})
	return sb.String(), rows
}

// unpaidCount is how many of the chat's orders still wait for payment.
func (s *Server) unpaidCount(chat int64) int64 {
	if chat == 0 {
		return 0
	}
	var n int64
	s.st.DB.Model(&store.Order{}).Where("telegram_id = ? AND status = ? AND expires_at > ?", chat, "pending", time.Now()).Count(&n)
	return n
}

// unpaidView lists the chat's open orders with buttons to pay or cancel.
func (b *tgBot) unpaidView(chat int64) (string, [][]button) {
	var os []store.Order
	b.s.st.DB.Where("telegram_id = ? AND status = ? AND expires_at > ?", chat, "pending", time.Now()).Order("created_at desc").Find(&os)
	var sb strings.Builder
	sb.WriteString("⏳ <b>未付款订单</b>\n" + tgRule + "\n")
	if len(os) == 0 {
		sb.WriteString("没有待支付的订单。")
	}
	var rows [][]button
	for i, o := range os {
		n := strconv.Itoa(i + 1)
		mins := int(time.Until(o.ExpiresAt).Round(time.Minute).Minutes())
		fmt.Fprintf(&sb, "\n%s. <b>%s</b> · %s · %s\n<code>%s</code> · 剩 %d 分钟\n", n, esc(o.PlanName), esc(orderAmount(&o)), esc(payMethodName[o.Method]), o.ID, mins)
		var row []button
		if b.s.methodOn(o.Method) {
			row = append(row, button{Text: "💳 继续支付 " + n, Data: "s:r:" + o.ID})
		}
		if o.Method != payStars {
			row = append(row, button{Text: "🔄 查询 " + n, Data: "s:q:" + o.ID})
		}
		rows = append(rows, append(row, button{Text: "❌ 取消 " + n, Data: "s:c:" + o.ID}))
	}
	rows = append(rows, []button{{Text: "📋 我的订单", Data: "v:orders"}, {Text: "« 商店", Data: "v:shop"}})
	return sb.String(), rows
}

var payMethodName = map[string]string{payStars: "Stars", payEPay: "在线支付", payUSDT: "USDT", payCode: "兑换码", payManual: "手动", payCoupon: "优惠券"}

var orderStatusText = map[string]string{"pending": "⏳ 待支付", "processing": "🔄 已收款待交付", "refund_pending": "↩️ 退款处理中", "paid": "✅ 已支付", "expired": "⌛ 已过期", "failed": "❗ 开通失败", "refunded": "↩️ 已退款", "cancelled": "🚫 已取消"}

func redeemHelp() string {
	return "🎟 <b>兑换码</b>\n" + tgRule + "\n发送：\n<code>/redeem 兑换码</code>\n\n兑换后立即开通对应套餐（没有账号会自动创建），或给账号加上流量包。"
}

func (b *tgBot) planCard(id tgIdentity, planID string) (string, [][]button) {
	var p store.Group
	if !b.s.shopOn() || b.s.st.DB.First(&p, "id = ? AND plan = ? AND sale = ?", planID, true, true).Error != nil {
		return "这个套餐已下架。", [][]button{{{Text: "« 返回", Data: "v:shop"}, btnHome}}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🛒 <b>%s</b>\n%s\n%s\n", esc(p.Name), tgRule, esc(b.s.tgPlanLine(&p)))
	if p.Price != "" {
		sb.WriteString("💴 " + esc(p.Price) + "\n")
	}
	switch {
	case id.user == nil:
		if b.s.shopAutoCreate() {
			sb.WriteString("\n付款后自动为你创建账号，并发送订阅链接。")
		} else {
			sb.WriteString("\n请先绑定账号再购买。")
		}
	case id.user.PlanID == p.ID:
		if p.Days > 0 {
			fmt.Fprintf(&sb, "\n续费：到期时间顺延 %d 天，已用流量保留。", p.Days)
		} else {
			sb.WriteString("\n你已是这个套餐。")
		}
	default:
		sb.WriteString("\n⚠️ 开通后从今天开始计时，<b>已用流量清零</b>，原套餐立即失效。")
	}
	cp, code, note := b.s.botCoupon(id.chat, "plan", p.ID, botBuyer(id))
	sb.WriteString(couponLine(cp, code, note))
	sb.WriteString("\n\n选择付款方式（付款即表示同意<b>购买条款</b>，见 /terms）：")
	var rows [][]button
	if id.user != nil || b.s.shopAutoCreate() {
		rows = b.s.priceButtons(cp, "s:m:"+p.ID, p.PriceStars, p.PriceCNY, p.PriceUSDT)
	}
	if len(rows) == 0 {
		text := sb.String()
		text = text[:strings.LastIndex(text, "\n\n选择付款方式")]
		if !b.s.codesOn() {
			return text + "\n\n暂未开放付款方式，请联系管理员。", [][]button{{{Text: "« 返回", Data: "v:shop"}, btnHome}}
		}
		return text + "\n\n🎟 这个套餐通过兑换码开通：取得兑换码后发送 <code>/redeem 兑换码</code>。", [][]button{
			{{Text: "🎟 兑换码", Data: "v:redeem"}}, {{Text: "« 返回", Data: "v:shop"}, btnHome}}
	}
	rows = append(rows, []button{{Text: "« 返回", Data: "v:shop"}, btnHome})
	return sb.String(), rows
}

func tgName(u tgUser) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return u.FirstName
}

// shopCallback handles s:… callbacks.
func (b *tgBot) shopCallback(ctx context.Context, parts []string, chat, msg int64, from tgUser, id tgIdentity, answer func(string)) {
	if len(parts) < 3 {
		answer("")
		return
	}
	switch parts[1] {
	case "p":
		text, rows := b.planCard(id, parts[2])
		b.edit(chat, msg, text, rows...)
		answer("")
	case "m":
		if len(parts) < 4 {
			answer("")
			return
		}
		userID := ""
		typ := ""
		if len(parts) > 4 {
			typ = parts[4]
		}
		chain := usdtChainArg(parts[3], typ)
		typ, err := b.s.paymentType(parts[3], typ)
		if err != nil {
			answer(err.Error())
			return
		}
		if id.user != nil {
			userID = id.user.ID
		}
		o, err := b.s.newOrderFor(shopBuyer{TGID: chat, TGName: tgName(from), UserID: userID, Coupon: b.s.tgCoupon(chat)}, parts[2], parts[3], chain)
		if err != nil {
			answer(err.Error())
			return
		}
		b.afterOrder(ctx, chat, msg, o, typ, answer)
	case "k": // traffic pack card
		text, rows := b.packCard(id, parts[2])
		b.edit(chat, msg, text, rows...)
		answer("")
	case "n": // s:n:<pack>:<method>[:<type>] new pack order
		if len(parts) < 4 {
			answer("")
			return
		}
		typ := ""
		if len(parts) > 4 {
			typ = parts[4]
		}
		chain := usdtChainArg(parts[3], typ)
		typ, err := b.s.paymentType(parts[3], typ)
		if err != nil {
			answer(err.Error())
			return
		}
		o, err := b.s.newPackOrderFor(shopBuyer{TGID: chat, TGName: tgName(from), Coupon: b.s.tgCoupon(chat)}, id.user, parts[2], parts[3], chain)
		if err != nil {
			answer(err.Error())
			return
		}
		b.afterOrder(ctx, chat, msg, o, typ, answer)
	case "q":
		var o store.Order
		if b.s.st.DB.First(&o, "id = ? AND telegram_id = ?", parts[2], chat).Error != nil {
			answer("订单不存在")
			return
		}
		if o.Status != "paid" && o.Method == payUSDT {
			b.s.pollUSDT()
			b.s.st.DB.First(&o, "id = ?", o.ID)
		}
		switch o.Status {
		case "processing":
			answer("已收到付款，正在重试交付，请勿重复付款")
		case "refund_pending":
			answer("退款处理中，正在核验结果")
		case "paid":
			answer("已支付，套餐已开通")
		case "failed":
			answer("已付款但开通失败，请联系管理员")
		case "expired":
			answer("订单已过期；如已付款，到账后仍会自动开通")
		case "cancelled":
			answer("订单已取消；如已付款，到账后仍会自动开通")
		case "refunded":
			answer("订单已退款")
		default:
			answer("还没有收到付款，请稍后再查")
		}
	case "r": // s:r:<order>[:<type>] pay an open order again
		b.resumeOrder(ctx, parts, chat, msg, answer)
	case "c": // s:c:<order> ask before cancelling
		var o store.Order
		if b.s.st.DB.First(&o, "id = ? AND telegram_id = ?", parts[2], chat).Error != nil {
			answer("订单不存在")
			return
		}
		if o.Status != "pending" {
			answer("订单" + orderStatusName[o.Status] + "，无法取消")
			return
		}
		b.edit(chat, msg, fmt.Sprintf("❌ <b>取消订单</b>\n%s\n<code>%s</code>\n%s · %s\n\n确定取消吗？取消后请不要再付款。", tgRule, o.ID, esc(o.PlanName), esc(orderAmount(&o))),
			[]button{{Text: "✅ 确定取消", Data: "s:x:" + o.ID}, {Text: "« 继续支付", Data: "s:r:" + o.ID}})
		answer("")
	case "x": // s:x:<order> cancel
		o, err := b.s.cancelOrder(chat, parts[2])
		if err != nil {
			answer(err.Error())
			return
		}
		answer("订单已取消")
		b.edit(chat, msg, fmt.Sprintf("🚫 <b>订单已取消</b>\n%s\n<code>%s</code>\n%s · %s\n\n本订单已关闭，请勿继续付款。如取消前已完成扣款，将按真实到账处理。", tgRule, o.ID, esc(o.PlanName), esc(orderAmount(o))),
			[]button{{Text: "⏳ 未付款订单", Data: "v:unpaid"}, {Text: "« 商店", Data: "v:shop"}})
	default:
		answer("")
	}
}

// afterOrder sends the payment of a new order; one the coupon made free
// is already delivered (orderDelivered announces it).
func (b *tgBot) afterOrder(ctx context.Context, chat, msg int64, o *store.Order, typ string, answer func(string)) {
	if o.CouponCode != "" {
		var old string
		b.s.eph.take(tgCouponKey(chat), &old) // used: a later purchase starts without it
	}
	if o.Status == "paid" {
		answer("已用优惠码免费开通")
		return
	}
	answer("")
	b.sendPayment(ctx, chat, msg, o, typ)
}

// resumeOrder sends the payment of an open order again. An EPay order
// first asks which way to pay when the shop offers several.
func (b *tgBot) resumeOrder(ctx context.Context, parts []string, chat, msg int64, answer func(string)) {
	var o store.Order
	if b.s.st.DB.First(&o, "id = ? AND telegram_id = ?", parts[2], chat).Error != nil {
		answer("订单不存在")
		return
	}
	switch {
	case o.Status != "pending":
		answer("订单" + orderStatusName[o.Status] + "，请重新下单")
		return
	case !time.Now().Before(o.ExpiresAt):
		answer("订单已过期，请重新下单")
		return
	case !b.s.shopOn():
		answer(errShopClosed.Error())
		return
	case !b.s.methodOn(o.Method):
		answer("这个付款方式已停用，请重新下单")
		return
	}
	typ := ""
	if len(parts) > 3 {
		typ = parts[3]
	}
	if o.Method == payEPay && typ == "" {
		types := b.s.epayTypes()
		if len(types) > 1 {
			var row []button
			for _, t := range types {
				row = append(row, button{Text: orDefault(epayTypeName[t], t), Data: "s:r:" + o.ID + ":" + t})
			}
			b.edit(chat, msg, fmt.Sprintf("💳 <b>继续支付</b>\n%s\n<code>%s</code>\n%s · ¥%s\n\n选择付款方式：", tgRule, o.ID, esc(o.PlanName), o.Amount),
				row, []button{{Text: "« 返回", Data: "v:unpaid"}})
			answer("")
			return
		}
		if len(types) == 1 {
			typ = types[0]
		}
	}
	typ, err := b.s.paymentType(o.Method, typ)
	if err != nil {
		answer(err.Error())
		return
	}
	answer("")
	b.sendPayment(ctx, chat, msg, &o, typ)
}

// orderDelivered tells the buyer the plan is on.
func (b *tgBot) orderDelivered(o *store.Order, u *store.User, created bool) {
	chat := o.TelegramID
	if chat == 0 {
		chat = u.TelegramID
	}
	if chat == 0 || !b.isRunning() {
		return
	}
	text := "✅ <b>支付成功</b>\n" + tgRule + "\n" + esc(o.Detail)
	if created {
		text += "\n\n已为你创建账号 <b>" + esc(u.Username) + "</b> 并绑定到这个 Telegram。"
	}
	b.send(chat, text+"\n\n"+b.userCard(u), b.userButtons(u)...)
	if created {
		b.sendSub(chat, u)
	}
}

// redeemCommand handles /redeem <code>.
func (b *tgBot) redeemCommand(chat int64, from tgUser, arg string, id tgIdentity) {
	if !b.s.codesOn() {
		b.send(chat, "兑换码兑换已停用，请联系管理员。", backRow(""))
		return
	}
	if arg == "" {
		b.send(chat, redeemHelp(), backRow(""))
		return
	}
	userID := ""
	if id.user != nil {
		userID = id.user.ID
	}
	if _, _, err := b.s.redeem(chat, tgName(from), userID, arg); err != nil {
		b.send(chat, "❌ "+esc(err.Error()), backRow(""))
	}
	// Success is announced by orderDelivered.
}

// packsView lists the traffic packs the user may buy.
func (b *tgBot) packsView(id tgIdentity) (string, [][]button) {
	var sb strings.Builder
	sb.WriteString("📦 <b>流量包</b>\n" + tgRule + "\n流量不够用时加购，不改变套餐和到期时间；<b>本流量周期内有效</b>，下次流量重置或续费新周期时失效。\n")
	var rows [][]button
	for _, p := range b.s.salePacks(id.user) {
		prices := b.s.productPrices(p.PriceStars, p.PriceCNY, p.PriceUSDT)
		if prices == "" {
			continue
		}
		fmt.Fprintf(&sb, "\n<b>%s</b>  +%s\n%s\n", esc(p.Name), sub.HumanBytes(p.Traffic), esc(prices))
		rows = append(rows, []button{{Text: p.Name + " · +" + sub.HumanBytes(p.Traffic), Data: "s:k:" + p.ID}})
	}
	if len(rows) == 0 {
		sb.WriteString("\n暂时没有可购买的流量包。")
	}
	rows = append(rows, []button{{Text: "« 返回", Data: "v:shop"}, btnHome})
	return sb.String(), rows
}

// packCard offers the ways to pay for one pack.
func (b *tgBot) packCard(id tgIdentity, packID string) (string, [][]button) {
	back := [][]button{{{Text: "« 返回", Data: "v:packs"}, btnHome}}
	var p store.TrafficPack
	if !b.s.shopOn() || b.s.st.DB.First(&p, "id = ? AND sale = ?", packID, true).Error != nil {
		return "这个流量包已下架。", back
	}
	if err := packFor(&p, id.user); err != nil {
		return esc(err.Error()), back
	}
	text := fmt.Sprintf("📦 <b>%s</b>\n%s\n本周期增加 <b>%s</b> 流量，下次流量重置或开始新周期时失效。\n\n选择付款方式（付款即表示同意<b>购买条款</b>，见 /terms）：",
		esc(p.Name), tgRule, sub.HumanBytes(p.Traffic))
	cp, code, note := b.s.botCoupon(id.chat, kindPack, p.ID, botBuyer(id))
	if l := couponLine(cp, code, note); l != "" {
		text = strings.Replace(text, "\n\n选择付款方式", l+"\n\n选择付款方式", 1)
	}
	rows := b.s.priceButtons(cp, "s:n:"+p.ID, p.PriceStars, p.PriceCNY, p.PriceUSDT)
	return text, append(rows, back...)
}

// usdtChainArg is the USDT network in a buy callback's type slot.
func usdtChainArg(method, typ string) string {
	if method == payUSDT {
		return typ
	}
	return ""
}

// priceButtons are a product's buy buttons (prefix s:m:<plan> or
// s:n:<pack>), with the coupon's prices when one applies.
func (s *Server) priceButtons(cp *store.Coupon, prefix string, stars int, cny, usdt float64) [][]button {
	var rows [][]button
	label := func(orig, a string, free bool) string {
		switch {
		case free:
			return orig + " → 免费"
		case cp != nil && a != orig:
			return orig + " → " + a
		}
		return a
	}
	if a, orig, _, _, free, ok := s.couponPrice(cp, stars, cny, usdt, payStars); ok {
		rows = append(rows, []button{{Text: "⭐ " + label(orig, a, free) + " Stars", Data: prefix + ":" + payStars}})
	}
	if a, orig, _, _, free, ok := s.couponPrice(cp, stars, cny, usdt, payEPay); ok {
		var row []button
		for _, t := range s.epayTypes() {
			row = append(row, button{Text: orDefault(epayTypeName[t], t) + " ¥" + label(orig, a, free), Data: prefix + ":" + payEPay + ":" + t})
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	if a, _, _, _, free, ok := s.couponPrice(cp, stars, cny, usdt, payUSDT); ok {
		shown := strconv.FormatFloat(usdt, 'f', -1, 64)
		if cp != nil {
			shown = label(shown, strings.TrimRight(strings.TrimRight(a, "0"), "."), free)
		}
		rows = append(rows, s.usdtButtonsLabel(prefix, shown)...)
	}
	return rows
}

// couponLine is the card's note about the chat's coupon.
func couponLine(cp *store.Coupon, code, note string) string {
	switch {
	case cp != nil:
		return "\n🏷 优惠码 <code>" + esc(code) + "</code>：" + esc(couponLabel(cp))
	case note != "":
		return "\n🏷 优惠码 <code>" + esc(code) + "</code> 不可用：" + esc(note)
	}
	return ""
}

// botBuyer is the buyer behind a bot identity.
func botBuyer(id tgIdentity) shopBuyer {
	b := shopBuyer{TGID: id.chat}
	if id.user != nil {
		b.UserID = id.user.ID
	}
	return b
}

func (s *Server) usdtButtonsLabel(prefix, price string) [][]button {
	var rows [][]button
	var row []button
	for _, n := range s.usdtNetsOn() {
		row = append(row, button{Text: "💲 USDT · " + n.Short + " " + price, Data: prefix + ":" + payUSDT + ":" + n.ID})
		if len(row) == 2 {
			rows, row = append(rows, row), nil
		}
	}
	if row != nil {
		rows = append(rows, row)
	}
	return rows
}
