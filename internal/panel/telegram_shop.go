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
// s:m:<plan>:<method>[:<type>] new order, s:q:<order> payment check.

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
			continue
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
	rows = append(rows, []button{{Text: "🎟 兑换码", Data: "v:redeem"}, {Text: "📋 我的订单", Data: "v:orders"}})
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

// ordersView lists the chat's recent orders.
func (b *tgBot) ordersView(chat int64) (string, [][]button) {
	var os []store.Order
	b.s.st.DB.Where("telegram_id = ?", chat).Order("created_at desc").Limit(10).Find(&os)
	var sb strings.Builder
	sb.WriteString("📋 <b>我的订单</b>\n" + tgRule + "\n")
	if len(os) == 0 {
		sb.WriteString("还没有订单。")
	}
	var rows [][]button
	for _, o := range os {
		amount := o.Amount + " " + o.Currency
		switch o.Method {
		case payCode:
			amount = "兑换码"
		case payStars:
			amount = o.Amount + " ⭐"
		case payEPay:
			amount = "¥" + o.Amount
		}
		fmt.Fprintf(&sb, "\n<code>%s</code> %s\n%s · %s · %s\n", o.ID, orderStatusText[o.Status], esc(o.PlanName), esc(amount), o.CreatedAt.Format("01-02 15:04"))
		if o.Status == "pending" && o.Method != payStars {
			rows = append(rows, []button{{Text: "🔄 查询 " + o.ID, Data: "s:q:" + o.ID}})
		}
	}
	rows = append(rows, []button{{Text: "« 返回", Data: "v:shop"}, btnHome})
	return sb.String(), rows
}

var orderStatusText = map[string]string{"pending": "⏳ 待支付", "paid": "✅ 已支付", "expired": "⌛ 已过期", "failed": "❗ 开通失败", "refunded": "↩️ 已退款"}

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
	sb.WriteString("\n\n选择付款方式（付款即表示同意<b>购买条款</b>，见 /terms）：")
	var rows [][]button
	if id.user != nil || b.s.shopAutoCreate() {
		if a, _, ok := b.s.orderPrice(&p, payStars); ok {
			rows = append(rows, []button{{Text: "⭐ " + a + " Stars", Data: "s:m:" + p.ID + ":" + payStars}})
		}
		if a, _, ok := b.s.orderPrice(&p, payEPay); ok {
			var row []button
			for _, t := range b.s.epayTypes() {
				row = append(row, button{Text: orDefault(epayTypeName[t], t) + " ¥" + a, Data: "s:m:" + p.ID + ":" + payEPay + ":" + t})
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
		}
		if _, _, ok := b.s.orderPrice(&p, payUSDT); ok {
			rows = append(rows, []button{{Text: "💲 USDT (TRC20) " + strconv.FormatFloat(p.PriceUSDT, 'f', -1, 64), Data: "s:m:" + p.ID + ":" + payUSDT}})
		}
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
		if id.user != nil {
			userID = id.user.ID
		}
		o, err := b.s.newOrder(chat, tgName(from), userID, parts[2], parts[3])
		if err != nil {
			answer(err.Error())
			return
		}
		answer("")
		typ := ""
		if len(parts) > 4 {
			typ = parts[4]
		}
		b.sendPayment(ctx, chat, o, typ)
	case "k": // traffic pack card
		text, rows := b.packCard(id, parts[2])
		b.edit(chat, msg, text, rows...)
		answer("")
	case "n": // s:n:<pack>:<method>[:<type>] new pack order
		if len(parts) < 4 {
			answer("")
			return
		}
		o, err := b.s.newPackOrder(chat, tgName(from), id.user, parts[2], parts[3])
		if err != nil {
			answer(err.Error())
			return
		}
		answer("")
		typ := ""
		if len(parts) > 4 {
			typ = parts[4]
		}
		b.sendPayment(ctx, chat, o, typ)
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
		case "paid":
			answer("已支付，套餐已开通")
		case "failed":
			answer("已付款但开通失败，请联系管理员")
		case "expired":
			answer("订单已过期；如已付款，到账后仍会自动开通")
		default:
			answer("还没有收到付款，请稍后再查")
		}
	default:
		answer("")
	}
}

// onPreCheckout approves a Stars payment that matches its open order.
func (b *tgBot) onPreCheckout(q *tgPreCheckout) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reply := map[string]any{"pre_checkout_query_id": q.ID, "ok": true}
	var o store.Order
	switch {
	case b.s.st.DB.First(&o, "id = ? AND method = ?", q.Payload, payStars).Error != nil:
		reply["ok"], reply["error_message"] = false, "订单不存在"
	case o.Status != "pending" || time.Now().After(o.ExpiresAt):
		reply["ok"], reply["error_message"] = false, "订单已过期或已处理，请重新下单"
	case q.Currency != "XTR" || strconv.FormatInt(q.TotalAmount, 10) != o.Amount || q.From.ID != o.TelegramID:
		reply["ok"], reply["error_message"] = false, "订单信息不符"
	case !b.s.shopOn():
		reply["ok"], reply["error_message"] = false, "商店未开启"
	default:
		var n int64
		if o.Kind == kindPack {
			b.s.st.DB.Model(&store.TrafficPack{}).Where("id = ? AND sale = ?", o.PackID, true).Count(&n)
		} else {
			b.s.st.DB.Model(&store.Group{}).Where("id = ? AND plan = ? AND sale = ?", o.PlanID, true, true).Count(&n)
		}
		if n == 0 {
			reply["ok"], reply["error_message"] = false, "商品已下架"
		}
	}
	_ = b.call(ctx, "answerPreCheckoutQuery", reply, nil)
}

// onStarsPaid fulfils the order of a completed Stars payment.
func (b *tgBot) onStarsPaid(chat int64, p *tgPayment) {
	var o store.Order
	if b.s.st.DB.First(&o, "id = ? AND method = ?", p.Payload, payStars).Error != nil ||
		p.Currency != "XTR" || strconv.FormatInt(p.TotalAmount, 10) != o.Amount {
		b.s.log.Warn("stars payment without a matching order", "payload", p.Payload, "charge", p.ChargeID)
		b.send(chat, "已收到付款，但没有找到对应订单，请联系管理员（凭证："+esc(p.ChargeID)+"）")
		return
	}
	if _, _, _, err := b.s.fulfil(o.ID, "stars:"+p.ChargeID, nil); err != nil {
		b.send(chat, "已收到付款，但开通失败："+esc(err.Error())+"\n请联系管理员（订单 "+o.ID+"）")
	}
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
	b.send(chat, text+"\n\n"+b.userCard(u), userButtons()...)
	if created {
		b.sendSub(chat, u)
	}
}

// redeemCommand handles /redeem <code>.
func (b *tgBot) redeemCommand(chat int64, from tgUser, arg string, id tgIdentity) {
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

// sendPayment sends the way to pay an open order: a Stars invoice, an EPay
// link (typ = alipay, wxpay, …) or the USDT transfer details.
func (b *tgBot) sendPayment(ctx context.Context, chat int64, o *store.Order, typ string) {
	mins := int(time.Until(o.ExpiresAt).Round(time.Minute).Minutes())
	switch o.Method {
	case payStars:
		n, _ := strconv.Atoi(o.Amount)
		err := b.call(ctx, "sendInvoice", map[string]any{"chat_id": chat, "title": truncate(o.PlanName, 32),
			"description": truncate(b.s.st.GetSetting("site_title", "Polaris")+" · "+o.PlanName, 255), "payload": o.ID,
			"currency": "XTR", "prices": []map[string]any{{"label": truncate(o.PlanName, 32), "amount": n}}}, nil)
		if err != nil {
			b.send(chat, "发送账单失败："+esc(err.Error()))
		}
	case payEPay:
		if typ == "" {
			typ = payEPay
		}
		b.send(chat, fmt.Sprintf("🧾 <b>订单 %s</b>\n%s\n商品：%s\n金额：¥%s（%s）\n\n请在 %d 分钟内完成支付，付款后自动开通。",
			o.ID, tgRule, esc(o.PlanName), o.Amount, esc(orDefault(epayTypeName[typ], typ)), mins),
			[]button{{Text: "💳 去支付 ¥" + o.Amount, URL: b.s.epayLink(o, typ)}},
			[]button{{Text: "🔄 我已付款", Data: "s:q:" + o.ID}})
	case payUSDT:
		b.send(chat, fmt.Sprintf("🧾 <b>订单 %s</b>\n%s\n商品：%s\n网络：<b>TRON (TRC20)</b>\n地址：<code>%s</code>\n金额：<code>%s</code> USDT\n\n⚠️ 请<b>精确转账上面的金额</b>（含小数），金额不符无法自动到账。\n请在 %d 分钟内完成转账，到账确认后自动开通。",
			o.ID, tgRule, esc(o.PlanName), esc(b.s.st.GetSetting("shop_usdt_address", "")), o.Amount, mins),
			[]button{{Text: "🔄 我已付款", Data: "s:q:" + o.ID}})
	}
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
	var rows [][]button
	if a, _, ok := b.s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, payStars); ok {
		rows = append(rows, []button{{Text: "⭐ " + a + " Stars", Data: "s:n:" + p.ID + ":" + payStars}})
	}
	if a, _, ok := b.s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, payEPay); ok {
		var row []button
		for _, t := range b.s.epayTypes() {
			row = append(row, button{Text: orDefault(epayTypeName[t], t) + " ¥" + a, Data: "s:n:" + p.ID + ":" + payEPay + ":" + t})
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	if _, _, ok := b.s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, payUSDT); ok {
		rows = append(rows, []button{{Text: "💲 USDT (TRC20) " + strconv.FormatFloat(p.PriceUSDT, 'f', -1, 64), Data: "s:n:" + p.ID + ":" + payUSDT}})
	}
	return text, append(rows, back...)
}
