package asn

import (
	"bytes"
	"os"
	"testing"
)

func TestOfflineASNLookup(t *testing.T) {
	raw, err := os.ReadFile("testdata/GeoLite2-ASN-Test.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	db, err := FromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ip, state string
		number    uint32
	}{{"1.0.0.1", "matched", 15169}, {"2001:1700::1", "matched", 6730}, {"10.0.0.1", "private", 0}, {"100.64.0.1", "private", 0}, {"::1", "private", 0}, {"203.0.113.1", "not_found", 0}, {"invalid", "invalid", 0}, {"fe80::1%eth0", "invalid", 0}} {
		result := db.Lookup(tc.ip)
		if result.State != tc.state || result.ASN != tc.number {
			t.Fatal(tc, result)
		}
		if tc.number != 0 && (result.Organization == "" || result.Network == "") {
			t.Fatal(result)
		}
	}
	if db.Metadata.DatabaseType == "" || db.Metadata.BuildEpoch == 0 {
		t.Fatal(db.Metadata)
	}
}

func TestASNRejectsInvalidDatabase(t *testing.T) {
	raw, err := os.ReadFile("testdata/GeoLite2-ASN-Test.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte("not an MMDB"), raw[:len(raw)-20], bytes.ReplaceAll(raw, []byte("GeoLite2-ASN"), []byte("GeoLite2-Foo")), bytes.ReplaceAll(raw, []byte("autonomous_system_number"), bytes.Repeat([]byte("x"), len("autonomous_system_number")))} {
		if _, err := FromBytes(data); err == nil {
			t.Fatal("invalid database accepted")
		}
	}
}
