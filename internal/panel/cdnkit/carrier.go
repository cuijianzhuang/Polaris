package cdnkit

import (
	"errors"
	"strings"
)

// Preferred IPs by carrier.
//
// A CDN IP that is fast on China Telecom may be slow on China Mobile, so a
// profile with CarrierSplit hands each subscriber the IPs that speed-test
// points on the subscriber's own carrier reported. The carrier comes from,
// in order: ?carrier= on the subscription link, the user's choice in the
// user center, then the request's source IP looked up in the route ASN
// database (Settings → network checks). When none of these is known, or no
// fresh report from that carrier exists, the profile's merged list is used.

// Carriers: China Telecom, China Unicom, China Mobile.
var carriers = map[string]string{"ct": "电信", "cu": "联通", "cm": "移动"}

// ValidCarrier reports whether c is a known carrier code (ct / cu / cm).
func ValidCarrier(c string) bool {
	_, ok := carriers[c]
	return ok
}

// CarrierName is the Chinese name of a carrier code ("" when unknown).
func CarrierName(c string) string {
	return carriers[c]
}

// CarrierFromName reads a carrier from a speed-test point's name, e.g.
// "上海电信" or "Beijing Unicom".
func CarrierFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "电信") || strings.Contains(n, "telecom") || strings.Contains(n, "chinanet"):
		return "ct"
	case strings.Contains(n, "联通") || strings.Contains(n, "unicom"):
		return "cu"
	case strings.Contains(n, "移动") || strings.Contains(n, "mobile") || strings.Contains(n, "cmcc"):
		return "cm"
	}
	return ""
}

// ReportCarrier is the carrier a report counts for: the one it names, or
// the one its source name implies.
func ReportCarrier(carrier, source string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(carrier))
	if c == "" {
		return CarrierFromName(source), nil
	}
	if !ValidCarrier(c) {
		return "", errors.New("carrier 只能是 ct（电信）、cu（联通）、cm（移动）或留空")
	}
	return c, nil
}
