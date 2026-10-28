package panel

import (
	"regexp"
	"strings"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// One CDN hostname has one DNS record, so it can lead to one node only.
// Nodes sharing a profile each get their own hostname next to the origin
// domain: cdn.example.com → oracle-sg-cdn.example.com. It stays one label
// deep so Cloudflare's free certificate (*.example.com) covers it.

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// nodeCDNHost is the hostname for a node's entries behind a profile.
func nodeCDNHost(origin, nodeName string) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(nodeName), "-"), "-")
	if slug == "" {
		slug = "node"
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	label, rest, ok := strings.Cut(origin, ".")
	if !ok || !strings.Contains(rest, ".") {
		return slug + "." + origin // origin is the zone apex
	}
	return slug + "-" + label + "." + rest
}

// cdnHostFor picks the Host for a new cdn entry on nodeID: "" (the origin
// domain) unless another node already serves the origin domain, then the
// node's own hostname.
func cdnHostFor(db *gorm.DB, p *store.CDNProfile, nodeID string) string {
	var others int64
	db.Model(&store.Endpoint{}).Joins("JOIN inbounds ON inbounds.id = endpoints.inbound_id").
		Where("endpoints.cdn_profile_id = ? AND endpoints.kind = ? AND (endpoints.host = '' OR endpoints.host = ?) AND inbounds.node_id <> ?", p.ID, "cdn", p.OriginDomain, nodeID).
		Count(&others)
	if others == 0 {
		return ""
	}
	var node store.Node
	if db.Select("name").First(&node, "id = ?", nodeID).Error != nil {
		return ""
	}
	return nodeCDNHost(p.OriginDomain, node.Name)
}

// assignCDNHost gives a new cdn entry without a Host its node's hostname
// when another node already serves the profile's origin domain.
func assignCDNHost(db *gorm.DB, ep *store.Endpoint, nodeID string) {
	if ep.Kind != "cdn" || ep.Host != "" {
		return
	}
	var p store.CDNProfile
	if db.First(&p, "id = ?", ep.CDNProfileID).Error == nil {
		ep.Host = cdnHostFor(db, &p, nodeID)
	}
}

// splitSharedCDNHosts gives every node but the first its own hostname where
// several nodes' entries share one, and reports the changes.
func (s *Server) splitSharedCDNHosts(p *store.CDNProfile) []string {
	var eps []store.Endpoint
	s.st.DB.Where("cdn_profile_id = ? AND kind = ?", p.ID, "cdn").Order("created_at").Find(&eps)
	owner := map[string]string{} // host -> node id
	var notes []string
	for _, ep := range eps {
		var in store.Inbound
		if s.st.DB.Select("node_id", "tag").First(&in, "id = ?", ep.InboundID).Error != nil {
			continue
		}
		host := orDefault(ep.Host, p.OriginDomain)
		if prev, ok := owner[host]; !ok || prev == in.NodeID {
			owner[host] = in.NodeID
			continue
		}
		var node store.Node
		if s.st.DB.Select("name").First(&node, "id = ?", in.NodeID).Error != nil {
			continue
		}
		newHost := nodeCDNHost(p.OriginDomain, node.Name)
		if prev, ok := owner[newHost]; ok && prev != in.NodeID {
			continue // leave it for syncCDN to report
		}
		if s.st.DB.Model(&store.Endpoint{}).Where("id = ?", ep.ID).Update("host", newHost).Error == nil {
			owner[newHost] = in.NodeID
			notes = append(notes, in.Tag+" → "+newHost)
		}
	}
	if len(notes) > 0 {
		s.MarkDirty("cdn hosts split", s.nodesForCDNProfile(p.ID)...)
	}
	return notes
}

func (s *Server) nodesForCDNProfile(id string) []string {
	var inboundIDs []string
	s.st.DB.Model(&store.Endpoint{}).Where("cdn_profile_id = ?", id).Distinct().Pluck("inbound_id", &inboundIDs)
	if len(inboundIDs) == 0 {
		return nil
	}
	return s.nodesForInbounds(inboundIDs...)
}
