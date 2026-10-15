package panel

import (
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
	if err := s.st.DB.Create(&g).Error; err != nil {
		return err
	}
	var ins []string
	s.st.DB.Model(&store.Inbound{}).Where("service_only = ?", false).Pluck("id", &ins)
	for _, id := range ins {
		s.st.DB.Create(&store.GroupInbound{GroupID: g.ID, InboundID: id})
	}
	var users []string
	s.st.DB.Model(&store.User{}).Where("is_service = ? AND id NOT IN (?)", false,
		s.st.DB.Model(&store.UserAccess{}).Select("user_id")).Pluck("id", &users)
	for _, id := range users {
		s.st.DB.Create(&store.UserGroup{UserID: id, GroupID: g.ID})
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
