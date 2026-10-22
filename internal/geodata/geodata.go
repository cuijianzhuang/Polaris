// Package geodata lists the geosite and geoip categories routing rules can
// name, so the rule editor can browse them. Category names follow v2fly's
// domain-list-community and geoip, which Xray ships and SagerNet's
// sing-geosite / sing-geoip rule sets are built from.
//
// The geosite data is embedded in full (domains can be listed and looked
// up offline); for geoip only names and range counts are embedded.
//
//go:generate go run ./gen
package geodata

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
)

//go:embed geosite.dat.gz
var geositeGz []byte

//go:embed geoip.json
var geoipJSON []byte

// Domain types in geosite data.
const (
	TypePlain  = 0 // keyword
	TypeRegex  = 1
	TypeDomain = 2 // the domain and its subdomains
	TypeFull   = 3 // exact name
)

// Domain is one geosite entry.
type Domain struct {
	Type  int      `json:"type"`
	Value string   `json:"value"`
	Attrs []string `json:"attrs,omitempty"` // e.g. "cn", "ads"
}

// Site is a geosite category.
type Site struct {
	Name    string   `json:"name"`
	Domains []Domain `json:"-"`
}

// Category is a category as the browser lists it.
type Category struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Attrs []string `json:"attrs,omitempty"` // usable as name@attr
	// SingBox: a sing-box rule set exists for it (geoip: country codes
	// and "private" only).
	SingBox bool `json:"singBox"`
}

// IPCategory is a geoip category with its range counts.
type IPCategory struct {
	Name string `json:"name"`
	V4   int    `json:"v4"`
	V6   int    `json:"v6"`
}

var (
	loadOnce sync.Once
	sites    []Site
	byName   map[string]*Site
	loadErr  error
)

func load() {
	loadOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(geositeGz))
		if err != nil {
			loadErr = err
			return
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			loadErr = err
			return
		}
		sites, loadErr = ParseGeoSite(raw)
		byName = make(map[string]*Site, len(sites))
		for i := range sites {
			byName[sites[i].Name] = &sites[i]
		}
	})
}

// Sites returns every geosite category, sorted by name.
func Sites() ([]Category, error) {
	load()
	if loadErr != nil {
		return nil, loadErr
	}
	out := make([]Category, 0, len(sites))
	for _, s := range sites {
		c := Category{Name: s.Name, Count: len(s.Domains), SingBox: true}
		seen := map[string]bool{}
		for _, d := range s.Domains {
			for _, a := range d.Attrs {
				if !seen[a] {
					seen[a] = true
					c.Attrs = append(c.Attrs, a)
				}
			}
		}
		sort.Strings(c.Attrs)
		out = append(out, c)
	}
	return out, nil
}

// Entries returns a category's domains ("name" or "name@attr").
func Entries(name string) ([]Domain, error) {
	load()
	if loadErr != nil {
		return nil, loadErr
	}
	name, attr, _ := strings.Cut(strings.ToLower(strings.TrimSpace(name)), "@")
	s := byName[name]
	if s == nil {
		return nil, fmt.Errorf("没有名为 %s 的 geosite 分类", name)
	}
	if attr == "" {
		return s.Domains, nil
	}
	var out []Domain
	for _, d := range s.Domains {
		for _, a := range d.Attrs {
			if a == attr {
				out = append(out, d)
				break
			}
		}
	}
	return out, nil
}

// Lookup returns the categories with an entry matching host, sorted by name.
func Lookup(host string) ([]string, error) {
	load()
	if loadErr != nil {
		return nil, loadErr
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return nil, nil
	}
	var out []string
	for _, s := range sites {
		for _, d := range s.Domains {
			if d.matches(host) {
				out = append(out, s.Name)
				break
			}
		}
	}
	return out, nil
}

func (d Domain) matches(host string) bool {
	switch d.Type {
	case TypeFull:
		return host == d.Value
	case TypeDomain:
		return host == d.Value || strings.HasSuffix(host, "."+d.Value)
	case TypePlain:
		return strings.Contains(host, d.Value)
	case TypeRegex:
		re, err := regexp.Compile(d.Value)
		return err == nil && re.MatchString(host)
	}
	return false
}

// IPs returns the geoip categories, sorted by name.
func IPs() ([]IPCategory, error) {
	var out []IPCategory
	if err := json.Unmarshal(geoipJSON, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// IPSingBox reports whether sing-box has a rule set for a geoip category:
// SagerNet/sing-geoip carries country codes only; "private" is rendered as
// ip_is_private.
func IPSingBox(name string) bool {
	if name == "private" {
		return true
	}
	return len(name) == 2 && name[0] >= 'a' && name[0] <= 'z' && name[1] >= 'a' && name[1] <= 'z'
}

// --- protobuf (v2ray routercommon) ---
//
//	GeoSiteList { repeated GeoSite entry = 1; }
//	GeoSite     { string country_code = 1; repeated Domain domain = 2; }
//	Domain      { Type type = 1; string value = 2; repeated Attribute attribute = 3; }
//	Attribute   { string key = 1; oneof { bool bool_value = 2; int64 int_value = 3; } }
//	GeoIPList   { repeated GeoIP entry = 1; }
//	GeoIP       { string country_code = 1; repeated CIDR cidr = 2; }
//	CIDR        { bytes ip = 1; uint32 prefix = 2; }

var errTruncated = errors.New("geodata: truncated message")

// fields walks the top-level fields of a message.
func fields(b []byte, f func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error) error {
	for len(b) > 0 {
		num, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return errTruncated
		}
		b = b[l:]
		switch typ {
		case protowire.BytesType:
			v, l := protowire.ConsumeBytes(b)
			if l < 0 {
				return errTruncated
			}
			if err := f(num, typ, v, 0); err != nil {
				return err
			}
			b = b[l:]
		case protowire.VarintType:
			v, l := protowire.ConsumeVarint(b)
			if l < 0 {
				return errTruncated
			}
			if err := f(num, typ, nil, v); err != nil {
				return err
			}
			b = b[l:]
		default:
			l := protowire.ConsumeFieldValue(num, typ, b)
			if l < 0 {
				return errTruncated
			}
			b = b[l:]
		}
	}
	return nil
}

// ParseGeoSite decodes a geosite.dat, sorted by category name.
func ParseGeoSite(raw []byte) ([]Site, error) {
	var out []Site
	err := fields(raw, func(num protowire.Number, _ protowire.Type, entry []byte, _ uint64) error {
		if num != 1 {
			return nil
		}
		var s Site
		err := fields(entry, func(num protowire.Number, _ protowire.Type, v []byte, _ uint64) error {
			switch num {
			case 1:
				s.Name = strings.ToLower(string(v))
			case 2:
				var d Domain
				err := fields(v, func(num protowire.Number, _ protowire.Type, v []byte, n uint64) error {
					switch num {
					case 1:
						d.Type = int(n)
					case 2:
						d.Value = string(v)
					case 3:
						return fields(v, func(num protowire.Number, _ protowire.Type, v []byte, _ uint64) error {
							if num == 1 {
								d.Attrs = append(d.Attrs, string(v))
							}
							return nil
						})
					}
					return nil
				})
				if err != nil {
					return err
				}
				s.Domains = append(s.Domains, d)
			}
			return nil
		})
		if err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// ParseGeoIP decodes a geoip.dat into names and range counts.
func ParseGeoIP(raw []byte) ([]IPCategory, error) {
	var out []IPCategory
	err := fields(raw, func(num protowire.Number, _ protowire.Type, entry []byte, _ uint64) error {
		if num != 1 {
			return nil
		}
		var c IPCategory
		err := fields(entry, func(num protowire.Number, _ protowire.Type, v []byte, _ uint64) error {
			switch num {
			case 1:
				c.Name = strings.ToLower(string(v))
			case 2:
				return fields(v, func(num protowire.Number, _ protowire.Type, ip []byte, _ uint64) error {
					if num == 1 {
						if len(ip) == 4 {
							c.V4++
						} else {
							c.V6++
						}
					}
					return nil
				})
			}
			return nil
		})
		if err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}
