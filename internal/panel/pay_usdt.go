package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// USDT orders are paid by a plain TRC20 transfer to the shop's address. Each
// order has its own amount (uniqueUSDTAmount), and the panel polls TronGrid
// for confirmed incoming USDT transfers, matching them to orders by exact
// amount; the transaction id makes the payment count once.

const (
	usdtContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t" // Tether USD on TRON
	// usdtLookback is how long an order's amount stays reserved and late
	// transfers are still matched.
	usdtLookback = 3 * time.Hour
)

var usdtClient = &http.Client{Timeout: 15 * time.Second}

type trc20Transfer struct {
	TransactionID  string `json:"transaction_id"`
	BlockTimestamp int64  `json:"block_timestamp"`
	To             string `json:"to"`
	Value          string `json:"value"`
	TokenInfo      struct {
		Address  string `json:"address"`
		Decimals int    `json:"decimals"`
	} `json:"token_info"`
}

// usdtMicros turns "12.3456" into 12345600 (6 decimals).
func usdtMicros(amount string) (int64, bool) {
	f, err := strconv.ParseFloat(amount, 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return int64(math.Round(f * 1e6)), true
}

// pollUSDT matches recent transfers to open USDT orders.
func (s *Server) pollUSDT() {
	addr := strings.TrimSpace(s.st.GetSetting("shop_usdt_address", ""))
	if addr == "" {
		return
	}
	now := time.Now()
	var open []store.Order
	s.st.DB.Where("method = ? AND status IN ? AND created_at > ?", payUSDT, unpaidStatuses, now.Add(-usdtLookback)).
		Order("created_at").Find(&open)
	if len(open) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	txs, err := s.trc20Incoming(ctx, addr, open[0].CreatedAt.Add(-time.Minute))
	if err != nil {
		s.log.Warn("usdt poll", "err", err)
		return
	}
	for _, t := range txs {
		if !strings.EqualFold(t.TokenInfo.Address, usdtContract) || t.To != addr || t.TokenInfo.Decimals != 6 {
			continue
		}
		v, err := strconv.ParseInt(t.Value, 10, 64)
		if err != nil {
			continue
		}
		at := time.UnixMilli(t.BlockTimestamp)
		for _, o := range open {
			want, ok := usdtMicros(o.Amount)
			if !ok || want != v || at.Before(o.CreatedAt.Add(-time.Minute)) {
				continue
			}
			if _, _, _, err := s.fulfil(o.ID, "usdt:"+t.TransactionID, nil); err != nil {
				s.log.Warn("usdt fulfil", "order", o.ID, "tx", t.TransactionID, "err", err)
			}
			break
		}
	}
}

// trc20Incoming lists confirmed incoming TRC20 USDT transfers to addr since.
func (s *Server) trc20Incoming(ctx context.Context, addr string, since time.Time) ([]trc20Transfer, error) {
	base := strings.TrimRight(orDefault(s.st.GetSetting("shop_usdt_api", ""), "https://api.trongrid.io"), "/")
	q := url.Values{"only_to": {"true"}, "only_confirmed": {"true"}, "limit": {"200"}, "contract_address": {usdtContract},
		"min_timestamp": {strconv.FormatInt(since.UnixMilli(), 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/accounts/"+url.PathEscape(addr)+"/transactions/trc20?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key := s.st.Box.MustOpen(s.st.GetSetting("shop_usdt_apikey", "")); key != "" {
		req.Header.Set("TRON-PRO-API-KEY", key)
	}
	resp, err := usdtClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TronGrid HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data    []trc20Transfer `json:"data"`
		Success bool            `json:"success"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}
