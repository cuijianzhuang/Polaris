// Package geoip provides offline city-level attribution from local MMDB files.
// It has no network client or DNS resolver.
package geoip

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/netip"
	"strings"
	"unicode"

	"github.com/oschwald/maxminddb-golang"
)

const MaxDatabaseBytes = 128 << 20
const MaxCompressedBytes = 64 << 20

type Metadata struct {
	DatabaseType string `json:"databaseType"`
	BuildEpoch   uint   `json:"buildEpoch"`
	IPVersion    uint   `json:"ipVersion"`
}

type Match struct {
	State       string `json:"state"`
	IP          string `json:"ip"`
	CountryCode string `json:"countryCode,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryEN   string `json:"countryEn,omitempty"`
	Province    string `json:"province,omitempty"`
	ProvinceEN  string `json:"provinceEn,omitempty"`
	City        string `json:"city,omitempty"`
	CityEN      string `json:"cityEn,omitempty"`
	Network     string `json:"network,omitempty"`
}

type place struct {
	Code  string            `maxminddb:"iso_code"`
	Names map[string]string `maxminddb:"names"`
}
type record struct {
	Country      place   `maxminddb:"country"`
	Subdivisions []place `maxminddb:"subdivisions"`
	City         place   `maxminddb:"city"`
}
type Database struct {
	reader   *maxminddb.Reader
	Metadata Metadata
}

// DecodeUpload accepts raw MMDB or gzip by content, with independent bounds for
// compressed input and uncompressed output. gzip checksum/truncation errors fail.
func DecodeUpload(data []byte) ([]byte, error) { return decodeUpload(data, MaxDatabaseBytes) }
func decodeUpload(data []byte, limit int64) ([]byte, error) {
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("城市数据库不能为空，且不能超过 128 MiB")
	}
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	if len(data) > MaxCompressedBytes {
		return nil, errors.New("城市数据库压缩文件不能超过 64 MiB")
	}
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("无法读取城市数据库压缩文件")
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.New("城市数据库压缩文件损坏或不完整")
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("城市数据库解压后不能超过 128 MiB")
	}
	return raw, nil
}
func FromBytes(data []byte) (*Database, error) {
	if len(data) == 0 || len(data) > MaxDatabaseBytes {
		return nil, errors.New("城市数据库不能为空，且不能超过 128 MiB")
	}
	r, err := maxminddb.FromBytes(data)
	if err != nil {
		return nil, errors.New("无法读取城市 MMDB 数据库")
	}
	if len(r.Metadata.DatabaseType) > 128 || !strings.Contains(strings.ToLower(r.Metadata.DatabaseType), "city") || (r.Metadata.IPVersion != 4 && r.Metadata.IPVersion != 6) {
		return nil, errors.New("需要包含国家、省份、城市字段的 City 类型 MMDB 数据库")
	}
	if err := r.Verify(); err != nil {
		return nil, errors.New("城市 MMDB 完整性验证失败")
	}
	nets := r.Networks(maxminddb.SkipAliasedNetworks)
	compatible := false
	for i := 0; i < 64 && nets.Next(); i++ {
		var rec record
		if _, err := nets.Network(&rec); err != nil {
			return nil, errors.New("城市数据库字段格式不兼容")
		}
		if len(rec.Country.Names) > 0 || len(rec.City.Names) > 0 || len(rec.Subdivisions) > 0 {
			compatible = true
			break
		}
	}
	if !compatible {
		return nil, errors.New("数据库未包含兼容的地理位置字段")
	}
	return &Database{reader: r, Metadata: Metadata{DatabaseType: r.Metadata.DatabaseType, BuildEpoch: r.Metadata.BuildEpoch, IPVersion: r.Metadata.IPVersion}}, nil
}
func clean(s string) string {
	r := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	return string(r[:min(len(r), 128)])
}
func names(p place) (string, string) {
	en := clean(p.Names["en"])
	zh := clean(p.Names["zh-CN"])
	if zh == "" {
		zh = en
	}
	return zh, en
}

var carrierNAT = netip.MustParsePrefix("100.64.0.0/10")

func (db *Database) Lookup(value string) Match {
	out := Match{State: "invalid", IP: value}
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		out.IP = clean(value)
		return out
	}
	ip = ip.Unmap()
	out.IP = ip.String()
	if ip.IsPrivate() || !ip.IsGlobalUnicast() || carrierNAT.Contains(ip) {
		out.State = "private"
		return out
	}
	if ip.Is6() && db.Metadata.IPVersion == 4 {
		out.State = "not_found"
		return out
	}
	var rec record
	network, ok, err := db.reader.LookupNetwork(ip.AsSlice(), &rec)
	if err != nil {
		out.State = "unavailable"
		return out
	}
	if !ok {
		out.State = "not_found"
		return out
	}
	if len(rec.Country.Code) == 2 && rec.Country.Code[0] >= 'A' && rec.Country.Code[0] <= 'Z' && rec.Country.Code[1] >= 'A' && rec.Country.Code[1] <= 'Z' {
		out.CountryCode = rec.Country.Code
	}
	out.Country, out.CountryEN = names(rec.Country)
	out.City, out.CityEN = names(rec.City)
	if len(rec.Subdivisions) > 0 {
		out.Province, out.ProvinceEN = names(rec.Subdivisions[0])
	}
	if out.CountryCode == "" && out.Country == "" && out.Province == "" && out.City == "" {
		out.State = "not_found"
		return out
	}
	out.State = "matched"
	out.Network = network.String()
	return out
}
