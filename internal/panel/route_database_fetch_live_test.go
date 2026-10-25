package panel

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/asn"
	"github.com/cuijianzhuang/polaris/internal/geoip"
)

// Opt-in: production HTTPS, allowlist, metadata selection and real parsers.
// Repository tests in normal CI use deterministic fixtures.
func TestLiveRepositoryDatabase(t *testing.T) {
	if os.Getenv("POLARIS_DATABASE_REPOSITORY_TEST") == "" {
		t.Skip("set POLARIS_DATABASE_REPOSITORY_TEST=1 for repository downloads")
	}
	for _, tc := range []struct{ kind, source string }{{"asn", "loyalsoldier"}, {"geo", "wp-statistics"}, {"asn", "free-geoip"}} {
		t.Run(tc.source+"/"+tc.kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			client := newRepositoryDatabaseClient(nil)
			defer client.CloseIdleConnections()
			raw, err := downloadRepositoryDatabase(ctx, client, tc.kind, tc.source)
			if err != nil {
				t.Fatal(err)
			}
			if tc.kind == "asn" {
				db, err := asn.FromBytes(raw)
				if err != nil {
					t.Fatal(err)
				}
				match := db.Lookup("8.8.8.8")
				if match.State != "matched" || match.ASN == 0 || match.Organization == "" {
					t.Fatal(match)
				}
				t.Log(db.Metadata.DatabaseType, "verified")
			} else {
				raw, err = geoip.DecodeUpload(raw)
				if err != nil {
					t.Fatal(err)
				}
				db, err := geoip.FromBytes(raw)
				if err != nil {
					t.Fatal(err)
				}
				for _, ip := range []string{"81.2.69.160", "2001:218::1"} {
					match := db.Lookup(ip)
					if match.State != "matched" || match.Country == "" {
						t.Fatal(match)
					}
				}
				t.Log(db.Metadata.DatabaseType, "verified")
			}
		})
	}
}
