// Command gen refreshes the embedded geodata: v2fly's domain-list-community
// (geosite, embedded in full) and v2fly/geoip (names and range counts).
//
//	go generate ./internal/geodata
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/cuijianzhuang/polaris/internal/geodata"
)

const (
	geositeURL = "https://github.com/v2fly/domain-list-community/releases/latest/download/dlc.dat"
	geoipURL   = "https://github.com/v2fly/geoip/releases/latest/download/geoip.dat"
)

func fetch(url string) []byte {
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("%s: %s", url, resp.Status))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		fail(err)
	}
	return b
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "geodata gen:", err)
	os.Exit(1)
}

func main() {
	site := fetch(geositeURL)
	sites, err := geodata.ParseGeoSite(site)
	if err != nil || len(sites) < 100 {
		fail(fmt.Errorf("geosite looks wrong: %d categories, %v", len(sites), err))
	}
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(site)
	zw.Close()
	if err := os.WriteFile("geosite.dat.gz", gz.Bytes(), 0o644); err != nil {
		fail(err)
	}

	ips, err := geodata.ParseGeoIP(fetch(geoipURL))
	if err != nil || len(ips) < 100 {
		fail(fmt.Errorf("geoip looks wrong: %d categories, %v", len(ips), err))
	}
	b, _ := json.Marshal(ips)
	if err := os.WriteFile("geoip.json", b, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("geosite: %d categories, %d bytes gzipped; geoip: %d categories\n", len(sites), gz.Len(), len(ips))
}
