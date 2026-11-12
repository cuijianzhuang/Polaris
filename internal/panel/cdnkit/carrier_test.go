package cdnkit

import "testing"

func TestCarrierFromName(t *testing.T) {
	for name, want := range map[string]string{
		"上海电信": "ct", "Beijing Unicom": "cu", "广州移动 2": "cm",
		"ChinaNet 上海": "ct", "CMCC": "cm", "香港": "",
	} {
		if got := CarrierFromName(name); got != want {
			t.Errorf("CarrierFromName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestReportCarrier(t *testing.T) {
	if c, err := ReportCarrier("CM", "上海电信"); err != nil || c != "cm" {
		t.Fatalf("explicit carrier: %q %v", c, err)
	}
	if c, err := ReportCarrier("", "上海电信"); err != nil || c != "ct" {
		t.Fatalf("inferred carrier: %q %v", c, err)
	}
	if c, err := ReportCarrier("", "香港"); err != nil || c != "" {
		t.Fatalf("unknown source carrier: %q %v", c, err)
	}
	if _, err := ReportCarrier("att", "x"); err == nil {
		t.Fatal("unknown carrier accepted")
	}
	if !ValidCarrier("ct") || ValidCarrier("xx") || ValidCarrier("") {
		t.Fatal("ValidCarrier")
	}
	if CarrierName("ct") != "电信" || CarrierName("xx") != "" {
		t.Fatal("CarrierName")
	}
}
