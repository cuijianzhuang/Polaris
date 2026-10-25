package panel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"gorm.io/gorm"
)

func sourceEndpoint(n sub.Node) string {
	return fmt.Sprintf("%s|%s|%d|%s", n.Protocol, strings.ToLower(strings.TrimSuffix(strings.Trim(n.Server, "[]"), ".")), n.Port, n.Transport.Type)
}
func sourceConfig(n sub.Node) string {
	n.ResourceID, n.Name, n.Source, n.Region = "", "", "", ""
	b, _ := json.Marshal(n)
	return string(b)
}
func sourceChangeFields(a, b sub.Node) []string {
	fields := []string{}
	if a.Name != b.Name || a.Region != b.Region {
		fields = append(fields, "name")
	}
	if sourceEndpoint(a) != sourceEndpoint(b) {
		fields = append(fields, "endpoint")
	}
	if !reflect.DeepEqual(a.Transport, b.Transport) {
		fields = append(fields, "transport")
	}
	if !reflect.DeepEqual(a.TLS, b.TLS) {
		fields = append(fields, "tls")
	}
	if a.UUID != b.UUID || a.Password != b.Password || a.Username != b.Username || a.ObfsPassword != b.ObfsPassword || a.SnellPSK != b.SnellPSK || a.SnellUserKey != b.SnellUserKey {
		fields = append(fields, "credentials")
	}
	if sourceConfig(a) != sourceConfig(b) && len(fields) == 0 {
		fields = append(fields, "options")
	}
	return fields
}

// commitSourceRefresh reconciles identities and history atomically. HTTP fetches
// happen before the lock: a concurrent edit/refresh cannot overwrite newer data.
func (s *Server) commitSourceRefresh(src *store.SubSource, incoming []sub.Node, info store.SubTraffic) ([]sub.Node, error) {
	if len(incoming) > 5000 {
		return nil, fmt.Errorf("订阅源最多支持 5000 个节点")
	}
	now := time.Now()
	nodes := make([]sub.Node, 0, len(incoming))
	// Exact duplicate entries represent the same credential, not distinct resources.
	seen := map[string]bool{}
	for _, n := range incoming {
		n.ResourceID, n.Source = "", ""
		key := sourceConfig(n)
		if !seen[key] {
			nodes = append(nodes, n)
			seen[key] = true
		}
	}
	dirty := []string{}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := store.ReserveSourceResources(tx); e != nil {
			return e
		}
		var current store.SubSource
		if e := tx.First(&current, "id = ?", src.ID).Error; e != nil {
			return e
		}
		if current.Revision != src.Revision {
			return fmt.Errorf("订阅源已被修改或刷新，请重试")
		}
		var old []store.SubSourceNode
		if e := tx.Where("source_id = ?", src.ID).Order("missing, id").Find(&old).Error; e != nil {
			return e
		}
		oldNodes := make([]sub.Node, len(old))
		for i, r := range old {
			plain, e := s.st.Box.Open(r.Payload)
			if e != nil {
				return fmt.Errorf("节点凭据无法解密")
			}
			if e = json.Unmarshal([]byte(plain), &oldNodes[i]); e != nil {
				return fmt.Errorf("节点凭据记录无效")
			}
		}
		matches := make([]int, len(nodes))
		for i := range matches {
			matches[i] = -1
		}
		used := map[int]bool{}
		// Pre-index connection settings so large sources stay linear in node
		// count instead of repeatedly serializing the entire old list.
		byConfig := map[string][]int{}
		for j, o := range oldNodes {
			k := sourceConfig(o)
			byConfig[k] = append(byConfig[k], j)
		}
		for i, n := range nodes {
			k := sourceConfig(n)
			candidates := byConfig[k]
			if len(candidates) > 0 {
				j := candidates[0]
				matches[i] = j
				used[j] = true
				byConfig[k] = candidates[1:]
			}
		}
		// Only unambiguous unmatched endpoints can keep an ID during rotation.
		oldEndpoints, newEndpoints := map[string][]int{}, map[string][]int{}
		for j, o := range oldNodes {
			if !used[j] {
				k := sourceEndpoint(o)
				oldEndpoints[k] = append(oldEndpoints[k], j)
			}
		}
		for i, n := range nodes {
			if matches[i] < 0 {
				k := sourceEndpoint(n)
				newEndpoints[k] = append(newEndpoints[k], i)
			}
		}
		for k, is := range newEndpoints {
			js := oldEndpoints[k]
			if len(is) == 1 && len(js) == 1 {
				matches[is[0]] = js[0]
				used[js[0]] = true
			}
		}
		changes := []store.SubNodeChange{}
		unchanged := 0
		for i := range nodes {
			n := &nodes[i]
			r := store.SubSourceNode{ID: store.NewID(), SourceID: src.ID, Revision: 1}
			kind, fields := "added", []string(nil)
			if j := matches[i]; j >= 0 {
				r = old[j]
				fields = sourceChangeFields(oldNodes[j], *n)
				if r.Missing {
					kind = "restored"
				} else if len(fields) > 0 {
					kind = "updated"
				} else {
					kind = ""
					unchanged++
				}
				if kind != "" {
					r.Revision++
				}
			}
			n.ResourceID = r.ID
			b, e := json.Marshal(n)
			if e != nil {
				return e
			}
			r.Name, r.Protocol, r.Server, r.Port, r.Network, r.Security, r.Position, r.Missing = n.Name, string(n.Protocol), n.Server, n.Port, string(n.Transport.Type), string(n.TLS.Type), i, false
			if kind != "" {
				r.Payload = s.st.Box.Seal(string(b))
				changes = append(changes, store.SubNodeChange{ID: r.ID, Name: r.Name, Kind: kind, Fields: fields})
			}
			if e := tx.Save(&r).Error; e != nil {
				return e
			}
		}
		for j, r := range old {
			if !used[j] && !r.Missing {
				if e := tx.Model(&r).Updates(map[string]any{"missing": true, "revision": r.Revision + 1}).Error; e != nil {
					return e
				}
				changes = append(changes, store.SubNodeChange{ID: r.ID, Name: r.Name, Kind: "removed"})
			}
		}
		b, e := json.Marshal(nodes)
		if e != nil {
			return e
		}
		current.Cache, current.NodeCount, current.FetchedAt, current.LastError, current.UserInfo = s.st.Box.Seal(string(b)), len(nodes), &now, "", store.NewJSON(info)
		current.URL, current.Content = s.st.Box.Seal(current.URL), s.st.Box.Seal(current.Content)
		current.Revision++
		if e = tx.Save(&current).Error; e != nil {
			return e
		}
		h := store.SubSourceRefresh{ID: store.NewID(), SourceID: src.ID, Revision: current.Revision, Changes: store.NewJSON(changes), Unchanged: unchanged}
		if e = tx.Create(&h).Error; e != nil {
			return e
		}
		var stale []string
		if e = tx.Model(&store.SubSourceRefresh{}).Where("source_id = ?", src.ID).Order("revision DESC").Offset(100).Pluck("id", &stale).Error; e != nil {
			return e
		}
		if len(stale) > 0 {
			if e = tx.Delete(&store.SubSourceRefresh{}, "id IN ?", stale).Error; e != nil {
				return e
			}
		}
		// Re-publish also when an edited source becomes ready with identical
		// nodes: an earlier pending-parse spec may have blocked these references.
		if e = tx.Model(&store.Outbound{}).Where("source_node_id IN (?)", tx.Model(&store.SubSourceNode{}).Select("id").Where("source_id = ?", src.ID)).Distinct().Pluck("node_id", &dirty).Error; e != nil {
			return e
		}

		*src = current
		return nil
	})
	if err == nil && len(dirty) > 0 {
		sort.Strings(dirty)
		s.MarkDirty("subscription source refreshed", dirty...)
	}
	return nodes, err
}
