package model

// Geography and ASN use the same snapshot metadata format, independently.
type RouteTraceGeoMetadata = RouteTraceASNMetadata

type RouteTraceIPGeo struct {
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
