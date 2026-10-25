package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SubSourceNode is a durable upstream identity, retained when a source removes
// it. Payload includes all client credentials and is always sealed by the panel.
type SubSourceNode struct {
	ID        string    `gorm:"primaryKey;size:32" json:"id"`
	SourceID  string    `gorm:"size:32;index" json:"sourceId"`
	Name      string    `gorm:"type:text" json:"name"`
	Protocol  string    `gorm:"size:16" json:"protocol"`
	Server    string    `gorm:"size:255" json:"server"`
	Port      int       `json:"port"`
	Network   string    `gorm:"size:16" json:"network"`
	Security  string    `gorm:"size:16" json:"security"`
	Position  int       `json:"position"`
	Missing   bool      `gorm:"index" json:"missing"`
	Payload   string    `gorm:"type:text" json:"-"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SubNodeChange struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`             // added / removed / updated / restored
	Fields []string `json:"fields,omitempty"` // field names only, never credentials
}

// SubSourceRefresh contains metadata-only differences, never client configs.
type SubSourceRefresh struct {
	ID        string                `gorm:"primaryKey;size:32" json:"id"`
	SourceID  string                `gorm:"size:32;index" json:"sourceId"`
	Revision  int64                 `json:"revision"`
	Changes   JSON[[]SubNodeChange] `json:"changes"`
	Unchanged int                   `json:"unchanged"`
	CreatedAt time.Time             `json:"createdAt"`
}

// ReserveSourceResources serializes refreshes, bindings and deletion across
// replicas. Call before any configuration reads in a mutation transaction.
func ReserveSourceResources(tx *gorm.DB) error {
	if tx.Dialector.Name() == "postgres" {
		if e := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "polaris-source-resources").Error; e != nil {
			return e
		}
	}
	row := Setting{Key: "sub_source_resource_guard", Value: "1"}
	if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
		return e
	}
	return tx.Model(&Setting{}).Where("key = ?", row.Key).UpdateColumn("value", gorm.Expr("value")).Error
}

// SealLegacySubSources upgrades old databases before any request is served.
// Restore uses the same pass so older backups cannot reintroduce plaintext.
func SealLegacySubSources(db *gorm.DB, box *secret.Box) error {

	var rows []SubSource
	if e := db.Find(&rows).Error; e != nil {
		return e
	}
	if len(rows) > 0 && box == nil {
		return fmt.Errorf("subscription credentials require an encryption key")
	}
	for _, r := range rows {
		for col, v := range map[string]string{"url": r.URL, "content": r.Content, "cache": r.Cache} {
			if v == "" || strings.HasPrefix(v, "enc:v1:") {
				continue
			}
			if e := db.Model(&SubSource{}).Where("id = ? AND "+col+" = ?", r.ID, v).UpdateColumn(col, box.Seal(v)).Error; e != nil {
				return e
			}
		}
	}
	return nil
}
