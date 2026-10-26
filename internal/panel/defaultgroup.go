package panel

import (
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// A fresh install used to have no group at all, so a user and an inbound
// created through the UI were never connected and every subscription came
// back empty. The panel now keeps a default group that new users and
// inbounds join unless they are given groups (or bindings) explicitly.

const defaultGroupName = "默认"

// ensureDefaultGroup creates the default group when there is none. On an
// install that has never used groups, it also gathers existing inbounds and
// the users that have no access at all, which is the state that produced
// empty subscriptions.
func (s *Server) ensureDefaultGroup() error {
	var n int64
	s.st.DB.Model(&store.Group{}).Where("is_default = ?", true).Count(&n)
	if n > 0 {
		return nil
	}
	var total int64
	s.st.DB.Model(&store.Group{}).Count(&total)
	if total > 0 {
		// Groups are in use: mark the oldest one default rather than adding
		// a new group nobody asked for.
		var g store.Group
		if err := s.st.DB.Order("created_at").First(&g).Error; err != nil {
			return err
		}
		return s.st.DB.Model(&g).Update("is_default", true).Error
	}
	g := store.Group{ID: store.NewID(), Name: defaultGroupName, Remark: "新用户与新入站默认加入", IsDefault: true}
	var ins, users []string
	// One transaction: a failure leaves no half-filled group, and the next
	// start (still no groups) tries again.
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&g).Error; err != nil {
			return err
		}
		if err := tx.Model(&store.Inbound{}).Where("service_only = ?", false).Pluck("id", &ins).Error; err != nil {
			return err
		}
		gi := make([]store.GroupInbound, len(ins))
		for i, id := range ins {
			gi[i] = store.GroupInbound{GroupID: g.ID, InboundID: id}
		}
		if len(gi) > 0 {
			if err := tx.CreateInBatches(gi, 500).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&store.User{}).Where("is_service = ? AND id NOT IN (?)", false,
			tx.Model(&store.UserAccess{}).Select("user_id")).Pluck("id", &users).Error; err != nil {
			return err
		}
		ug := make([]store.UserGroup, len(users))
		for i, id := range users {
			ug[i] = store.UserGroup{UserID: id, GroupID: g.ID}
		}
		if len(ug) == 0 {
			return nil
		}
		return tx.CreateInBatches(ug, 500).Error
	})
	if err != nil {
		return err
	}
	if len(ins) > 0 && len(users) > 0 {
		s.log.Info("created the default group for existing users and inbounds", "inbounds", len(ins), "users", len(users))
		s.MarkAllDirty("default group created")
	}
	return nil
}

// defaultGroupIDs returns the default group(s) for new users and inbounds.
func (s *Server) defaultGroupIDs() []string {
	var ids []string
	s.st.DB.Model(&store.Group{}).Where("is_default = ?", true).Pluck("id", &ids)
	if ids == nil {
		ids = []string{}
	}
	return ids
}

func groupsOr(groups, def []string) []string {
	if len(groups) > 0 {
		return groups
	}
	return def
}
