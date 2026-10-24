// Package asn performs offline attribution using administrator-supplied MMDB
// data. It has no downloader, DNS resolver or external lookup service.
package asn

import (
	"errors"
	"net/netip"
	"strings"
	"unicode"

	"github.com/oschwald/maxminddb-golang"
)

const MaxDatabaseBytes = 64 << 20

type Metadata struct {
	DatabaseType string `json:"databaseType"`
	BuildEpoch   uint   `json:"buildEpoch"`
	IPVersion    uint   `json:"ipVersion"`
}

type Match struct {
	State        string `json:"state"`
	IP           string `json:"ip"`
	ASN          uint32 `json:"asn,omitempty"`
	Organization string `json:"organization,omitempty"`
	Network      string `json:"network,omitempty"`
}

type asnRecord struct {
	Number       uint32 `maxminddb:"autonomous_system_number"`
	Organization string `maxminddb:"autonomous_system_organization"`
}

var carrierNAT = netip.MustParsePrefix("100.64.0.0/10")

type Database struct {
	reader   *maxminddb.Reader
	Metadata Metadata
}

func FromBytes(data []byte) (*Database, error) {
	if len(data) == 0 || len(data) > MaxDatabaseBytes {
		return nil, errors.New("ASN 数据库不能为空，且不能超过 64 MiB")
	}
	r, err := maxminddb.FromBytes(data)
	if err != nil {
		return nil, errors.New("无法读取 MMDB 数据库")
	}
	if len(r.Metadata.DatabaseType) > 128 || !strings.Contains(strings.ToLower(r.Metadata.DatabaseType), "asn") || (r.Metadata.IPVersion != 4 && r.Metadata.IPVersion != 6) {
		return nil, errors.New("需要 ASN 类型的 IPv4/IPv6 MMDB 数据库")
	}
	if err := r.Verify(); err != nil {
		return nil, errors.New("MMDB 完整性验证失败")
	}
	networks := r.Networks(maxminddb.SkipAliasedNetworks)
	compatible := false
	for i := 0; i < 64 && networks.Next(); i++ {
		var record asnRecord
		if _, err := networks.Network(&record); err != nil {
			return nil, errors.New("ASN 数据库字段格式不兼容")
		}
		if record.Number > 0 {
			compatible = true
			break
		}
	}
	if !compatible {
		return nil, errors.New("数据库未包含兼容的 ASN 编号字段")
	}
	return &Database{reader: r, Metadata: Metadata{DatabaseType: r.Metadata.DatabaseType, BuildEpoch: r.Metadata.BuildEpoch, IPVersion: r.Metadata.IPVersion}}, nil
}

// The reader owns a byte slice, not an mmap or file descriptor; immutable
// snapshots can be shared while a replacement is installed and later GC'd.
func (db *Database) Lookup(value string) Match {
	out := Match{State: "invalid", IP: value}
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		if len(out.IP) > 64 {
			out.IP = out.IP[:64]
		}
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
	var record asnRecord
	network, ok, err := db.reader.LookupNetwork(ip.AsSlice(), &record)
	if err != nil {
		out.State = "unavailable"
		return out
	}
	if !ok || record.Number == 0 {
		out.State = "not_found"
		return out
	}
	out.State, out.ASN = "matched", record.Number
	name := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, record.Organization))
	out.Organization = string(name[:min(len(name), 256)])
	out.Network = network.String()
	return out
}
