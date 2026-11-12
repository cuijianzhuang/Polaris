package panel

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/panel/cdnkit"
	"github.com/cuijianzhuang/polaris/internal/panel/routedb"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// dbStore adapts a *gorm.DB (the panel database or a transaction) to the
// query interfaces of cdnkit and routedb. The SQL lives here; the
// selection, ranking and rebuild rules live in those packages.
type dbStore struct{ db *gorm.DB }

var (
	_ cdnkit.ReportStore   = dbStore{}
	_ routedb.SettingStore = dbStore{}
)

func (s dbStore) ReportsForPort(p *store.CDNProfile, port int, since time.Time, carrier string) ([]store.CDNIPReport, error) {
	q := s.db.Model(&store.CDNIPReport{}).Where("at > ?", since)
	q = q.Where("COALESCE(NULLIF(port,0),443) = ?", port).Where("COALESCE(NULLIF(scheme,''),'https') = ?", cdnkit.Scheme(p))
	if p.Provider == "cloudflare" {
		q = q.Where("profile_id IN (?)", s.db.Model(&store.CDNProfile{}).Select("id").Where("provider = ?", "cloudflare").Where("COALESCE(NULLIF(scheme, ''), 'https') = ?", cdnkit.Scheme(p)))
	} else {
		q = q.Where("profile_id = ?", p.ID)
	}
	if carrier != "" {
		q = q.Where("carrier = ?", carrier)
	}
	var rs []store.CDNIPReport
	err := q.Order("at DESC").Find(&rs).Error
	return rs, err
}

func (s dbStore) ProfilesSharingLists(origin *store.CDNProfile) ([]store.CDNProfile, error) {
	q := s.db.Where("id = ?", origin.ID)
	if origin.Provider == "cloudflare" {
		q = s.db.Where("provider = ?", "cloudflare").Where("COALESCE(NULLIF(scheme, ''), 'https') = ?", cdnkit.Scheme(origin))
	}
	var targets []store.CDNProfile
	err := q.Find(&targets).Error
	return targets, err
}

func (s dbStore) UpdateProfile(p *store.CDNProfile, updates map[string]any) error {
	return s.db.Model(p).Updates(updates).Error
}

func (s dbStore) ReportsByProfiles(ids []string) ([]store.CDNIPReport, error) {
	var rs []store.CDNIPReport
	err := s.db.Where("profile_id IN ?", ids).Order("at DESC").Find(&rs).Error
	return rs, err
}

func (s dbStore) Setting(key string) (string, bool, error) {
	var row store.Setting
	err := s.db.First(&row, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.Value, true, nil
}
