package cdnkit

import (
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// ReportStore is the database behind the report queries. cdnkit owns the
// selection, ranking and rebuild rules; the caller owns the SQL and the
// transaction, and adapts its *gorm.DB (the panel database or a
// transaction) to this interface. Only the queries the selection logic
// needs are exposed, so the logic can be tested against a fake.
type ReportStore interface {
	// ReportsForPort returns the raw reports filed under the profile's
	// scheme and port since a time, newest first, optionally of one
	// carrier. For a Cloudflare profile the reports of every Cloudflare
	// profile on the same scheme are included.
	ReportsForPort(p *store.CDNProfile, port int, since time.Time, carrier string) ([]store.CDNIPReport, error)
	// ProfilesSharingLists returns the profiles whose preferred lists
	// origin feeds: origin alone, or every Cloudflare profile on the
	// same scheme.
	ProfilesSharingLists(origin *store.CDNProfile) ([]store.CDNProfile, error)
	// UpdateProfile writes the given fields of a profile.
	UpdateProfile(p *store.CDNProfile, updates map[string]any) error
	// ReportsByProfiles returns the reports of the given profiles,
	// newest first.
	ReportsByProfiles(ids []string) ([]store.CDNIPReport, error)
}
