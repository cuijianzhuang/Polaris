package panel

import (
	"encoding/base64"
	"errors"
	"html/template"
	"net/url"
	"regexp"
	"slices"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// USDT can be paid on several networks. TRON (TRC20) takes a T… address;
// the EVM networks share one 0x address. Each network's open orders have
// their own unique amounts, and the professional edition polls each network
// for incoming transfers (TronGrid for TRON, eth_getLogs on the EVM ones).

type usdtNet struct {
	ID, Name, Short string
	EVM             bool
	ChainID         int64
	Contract        string // the USDT token contract on this network
	Decimals        int
	RPC             string  // default public JSON-RPC (EVM)
	BlockSecs       float64 // average block time, to estimate where to start scanning
	Confirmations   int64   // blocks before a transfer counts
	Explorer        string  // transaction page prefix
}

var usdtNets = []usdtNet{
	{ID: "tron", Name: "TRON (TRC20)", Short: "TRC20", Contract: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", Decimals: 6,
		Explorer: "https://tronscan.org/#/transaction/"},
	{ID: "bsc", Name: "BNB Smart Chain (BEP20)", Short: "BEP20", EVM: true, ChainID: 56, Contract: "0x55d398326f99059fF775485246999027B3197955",
		Decimals: 18, RPC: "https://bsc-dataseed.bnbchain.org", BlockSecs: 0.75, Confirmations: 15, Explorer: "https://bscscan.com/tx/"},
	{ID: "polygon", Name: "Polygon PoS", Short: "Polygon", EVM: true, ChainID: 137, Contract: "0xc2132D05D31c914a87C6611C10748AEb04B58e8F",
		Decimals: 6, RPC: "https://polygon-rpc.com", BlockSecs: 2, Confirmations: 64, Explorer: "https://polygonscan.com/tx/"},
	{ID: "arbitrum", Name: "Arbitrum One", Short: "Arbitrum", EVM: true, ChainID: 42161, Contract: "0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9",
		Decimals: 6, RPC: "https://arb1.arbitrum.io/rpc", BlockSecs: 0.25, Confirmations: 20, Explorer: "https://arbiscan.io/tx/"},
}

var (
	tronAddrRE = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)
	evmAddrRE  = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

func usdtNetByID(id string) (usdtNet, bool) {
	if id == "" {
		id = "tron" // orders from before multi-network USDT
	}
	i := slices.IndexFunc(usdtNets, func(n usdtNet) bool { return n.ID == id })
	if i < 0 {
		return usdtNet{}, false
	}
	return usdtNets[i], true
}

// usdtAddress is where USDT on network n is received ("" = not set up).
func (s *Server) usdtAddress(n usdtNet) string {
	if n.EVM {
		if a := strings.TrimSpace(s.st.GetSetting("shop_usdt_evm_address", "")); evmAddrRE.MatchString(a) {
			return a
		}
		return ""
	}
	return strings.TrimSpace(s.st.GetSetting("shop_usdt_address", ""))
}

// usdtRPC is network n's JSON-RPC endpoint (a custom one, or the default).
func (s *Server) usdtRPC(n usdtNet) string {
	return orDefault(strings.TrimSpace(s.st.GetSetting("shop_usdt_rpc_"+n.ID, "")), n.RPC)
}

// usdtNetsOn lists the networks that take orders: switched on and with an
// address. Before the network switch existed, TRON was the only one.
func (s *Server) usdtNetsOn() []usdtNet {
	if !s.payments() {
		return nil
	}
	enabled := strings.Split(s.st.GetSetting("shop_usdt_chains", "tron"), ",")
	var out []usdtNet
	for _, n := range usdtNets {
		if slices.Contains(enabled, n.ID) && s.usdtAddress(n) != "" {
			out = append(out, n)
		}
	}
	return out
}

// usdtNetFor picks the network of a new order: the one asked for when it is
// on, else the first one on.
func (s *Server) usdtNetFor(id string) (usdtNet, bool) {
	on := s.usdtNetsOn()
	for _, n := range on {
		if n.ID == id {
			return n, true
		}
	}
	if id == "" && len(on) > 0 {
		return on[0], true
	}
	return usdtNet{}, false
}

// usdtPayTo is the network and address an order is paid to.
func (s *Server) usdtPayTo(o *store.Order) (usdtNet, string) {
	n, ok := usdtNetByID(o.Chain)
	if !ok {
		return usdtNet{}, ""
	}
	return n, s.usdtAddress(n)
}

// qrPNG is a QR code of text as PNG.
func qrPNG(text string, size int) []byte {
	png, err := qrcode.Encode(text, qrcode.Medium, size)
	if err != nil {
		return nil
	}
	return png
}

// qrDataURL is a QR code of text for an <img src>.
func qrDataURL(text string) template.URL {
	png := qrPNG(text, 320)
	if png == nil {
		return ""
	}
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
}

func (s *Server) usdtChainIDs() []string {
	out := []string{}
	for _, n := range s.usdtNetsOn() {
		out = append(out, n.ID)
	}
	return out
}

// validUSDTSettings checks USDT addresses, networks and RPC endpoints before
// they are saved: a typo in an address sends buyers' money nowhere.
func validUSDTSettings(req map[string]string) error {
	if v := strings.TrimSpace(req["shop_usdt_address"]); v != "" && !tronAddrRE.MatchString(v) {
		return errors.New("TRON 收款地址格式不对（T 开头，34 位）")
	}
	if v := strings.TrimSpace(req["shop_usdt_evm_address"]); v != "" && !evmAddrRE.MatchString(v) {
		return errors.New("EVM 收款地址格式不对（0x 开头，42 位）")
	}
	if v, ok := req["shop_usdt_chains"]; ok && v != "" {
		for _, id := range strings.Split(v, ",") {
			if _, ok := usdtNetByID(id); !ok || id == "" {
				return errors.New("未知的 USDT 网络：" + id)
			}
		}
	}
	for _, n := range usdtNets {
		if v := strings.TrimSpace(req["shop_usdt_rpc_"+n.ID]); v != "" {
			if u, err := url.Parse(v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return errors.New(n.Name + " 的 RPC 地址应为 https://…")
			}
		}
	}
	return nil
}
