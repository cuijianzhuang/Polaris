package store

import (
	"github.com/cuijianzhuang/polaris/internal/sub"
	"time"
)

// Revision zero follows the group's latest version. Positive revisions pin an
// immutable version. Groups contain operators only, never sources or users.
type SubProcessingRef struct {
	GroupID  string `json:"groupId"`
	Revision int64  `json:"revision"`
}

type SubProcessingGroup struct {
	ID        string               `gorm:"primaryKey;size:32" json:"id"`
	Name      string               `gorm:"size:64;uniqueIndex" json:"name"`
	Remark    string               `gorm:"size:255" json:"remark"`
	Revision  int64                `json:"revision"`
	Operators JSON[[]sub.Operator] `json:"operators"`
	CreatedAt time.Time            `json:"createdAt"`
	UpdatedAt time.Time            `json:"updatedAt"`
}

type SubProcessingGroupVersion struct {
	GroupID   string               `gorm:"primaryKey;size:32" json:"groupId"`
	Revision  int64                `gorm:"primaryKey;autoIncrement:false" json:"revision"`
	Name      string               `gorm:"size:64" json:"name"`
	Remark    string               `gorm:"size:255" json:"remark"`
	Operators JSON[[]sub.Operator] `json:"operators"`
	CreatedAt time.Time            `json:"createdAt"`
}
