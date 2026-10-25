package geoip

import (
	"bytes"
	"compress/gzip"
	"os"
	"strings"
	"testing"
)

func cityFixture(t *testing.T) []byte {
	t.Helper()
	raw, e := os.ReadFile("testdata/GeoIP2-City-Test.mmdb")
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func zipped(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, e := w.Write(raw); e != nil {
		t.Fatal(e)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestCityLookup(t *testing.T) {
	db, e := FromBytes(cityFixture(t))
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ ip, state, cc, city string }{
		{"81.2.69.160", "matched", "GB", "London"},
		{"::ffff:81.2.69.160", "matched", "GB", "London"},
		{"2001:218::1", "matched", "JP", ""},
		{"1.1.1.1", "not_found", "", ""},
		{"10.0.0.1", "private", "", ""},
		{"100.64.0.1", "private", "", ""},
		{"::1", "private", "", ""},
		{"fe80::1%eth0", "invalid", "", ""},
		{"example.com", "invalid", "", ""},
	} {
		r := db.Lookup(tc.ip)
		if r.State != tc.state || r.CountryCode != tc.cc || r.CityEN != tc.city {
			t.Fatal(tc, r)
		}
		if tc.state == "matched" && (r.Country == "" || r.Network == "") {
			t.Fatal(r)
		}
	}
	r := db.Lookup("81.2.69.160")
	if r.ProvinceEN != "England" || r.Province == "" || r.City == "" {
		t.Fatal(r)
	}
	db.Metadata.IPVersion = 4
	if r := db.Lookup("2001:218::1"); r.State != "not_found" {
		t.Fatal(r)
	}
}
func TestCityUploadDecoding(t *testing.T) {
	raw := cityFixture(t)
	compressed := zipped(t, raw)
	for _, data := range [][]byte{raw, compressed} {
		got, e := DecodeUpload(data)
		if e != nil || !bytes.Equal(got, raw) {
			t.Fatal(e)
		}
	}
	for _, data := range [][]byte{nil, compressed[:len(compressed)-4], append(append([]byte{}, compressed[:len(compressed)-1]...), compressed[len(compressed)-1]^0xff), {0x1f, 0x8b, 0x00}} {
		if _, e := DecodeUpload(data); e == nil {
			t.Fatal("bad gzip accepted")
		}
	}
	if _, e := decodeUpload(zipped(t, bytes.Repeat([]byte("x"), 4096)), 512); e == nil {
		t.Fatal("decompression limit ignored")
	}
}
func TestCityRejectsWrongDatabase(t *testing.T) {
	raw := cityFixture(t)
	asn, e := os.ReadFile("../asn/testdata/GeoLite2-ASN-Test.mmdb")
	if e != nil {
		t.Fatal(e)
	}
	for _, data := range [][]byte{nil, []byte("bad"), raw[:len(raw)-20], asn, bytes.ReplaceAll(raw, []byte("GeoIP2-City"), []byte("GeoIP2-Fake"))} {
		if _, e := FromBytes(data); e == nil {
			t.Fatal("wrong city format accepted")
		}
	}
}
func TestCityNamesAreBounded(t *testing.T) {
	zh, en := names(place{Names: map[string]string{"en": "London\x00\n"}})
	if zh != "London" || en != "London" {
		t.Fatal(zh, en)
	}
	zh, en = names(place{Names: map[string]string{"en": strings.Repeat("e", 200), "zh-CN": strings.Repeat("中", 200)}})
	if len([]rune(zh)) != 128 || len(en) != 128 {
		t.Fatal(zh, en)
	}
}
