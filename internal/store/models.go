package store

import (
	"encoding/json"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Node is one managed server running an agent.
type Node struct {
	ID           string           `gorm:"primaryKey;size:32" json:"id"`
	Name         string           `gorm:"size:64;uniqueIndex" json:"name"`
	Address      string           `gorm:"size:255" json:"address"` // public host/IP used in subscriptions
	Domain       string           `gorm:"size:255" json:"domain"`  // optional, for ACME and {node.domain}
	Region       string           `gorm:"size:64" json:"region"`
	Flag         string           `gorm:"size:8" json:"flag"` // ISO country code for emoji flags
	Labels       JSON[[]string]   `json:"labels"`
	AgentVersion string           `gorm:"size:32" json:"agentVersion"`
	Status       string           `gorm:"size:16;index" json:"status"` // pending / online / offline / syncing / error
	LastSeen     *time.Time       `json:"lastSeen"`
	DesiredRev   int64            `json:"desiredRev"`
	AppliedRev   int64            `json:"appliedRev"`
	LastError    string           `gorm:"size:2000" json:"lastError"`
	DefaultCore  model.CoreType   `gorm:"size:16" json:"defaultCore"`
	TrafficRate  float64          `gorm:"default:1" json:"trafficRate"` // billing multiplier
	System       JSON[SystemInfo] `json:"system"`
	TokenHash    string           `gorm:"size:64;index" json:"-"`
	TokenExpires *time.Time       `json:"-"`
	CertSerial   string           `gorm:"size:64" json:"-"`
	Revoked      bool             `json:"revoked"`
	SortOrder    int              `json:"sortOrder"`
	DDNS         bool             `json:"ddns"` // follow the agent-detected public IP
	PublicIPv4   string           `gorm:"size:64" json:"publicIpv4"`
	PublicIPv6   string           `gorm:"size:64" json:"publicIpv6"`
	IPChangedAt  *time.Time       `json:"ipChangedAt"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

// SystemInfo is the latest heartbeat snapshot of a node.
type SystemInfo struct {
	CPU      float64 `json:"cpu"`
	Mem      float64 `json:"mem"`
	RxBps    uint64  `json:"rxBps"`
	TxBps    uint64  `json:"txBps"`
	Uptime   uint64  `json:"uptime"`
	Load1    float64 `json:"load1"`
	Hostname string  `json:"hostname"`
}

// Core is a core instance installed (or desired) on a node.
type Core struct {
	ID                string         `gorm:"primaryKey;size:32" json:"id"`
	NodeID            string         `gorm:"size:32;uniqueIndex:idx_core_node_type" json:"nodeId"`
	Type              model.CoreType `gorm:"size:16;uniqueIndex:idx_core_node_type" json:"type"`
	Version           string         `gorm:"size:32" json:"version"`
	Status            string         `gorm:"size:16" json:"status"` // running / stopped / installing / error
	PID               int            `gorm:"column:pid" json:"pid"`
	IsDefault         bool           `json:"isDefault"`
	StartedAt         *time.Time     `json:"startedAt"`
	Error             string         `gorm:"size:2000" json:"error"`
	InstalledVersions JSON[[]string] `json:"installedVersions"`
	Inbounds          int            `gorm:"-" json:"inbounds"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
}

// Inbound is the persisted form of model.Inbound.
type Inbound struct {
	ID               string                    `gorm:"primaryKey;size:32" json:"id"`
	NodeID           string                    `gorm:"size:32;index;uniqueIndex:idx_inbound_node_tag" json:"nodeId"`
	Core             model.CoreType            `gorm:"size:16" json:"core"`
	Tag              string                    `gorm:"size:64;uniqueIndex:idx_inbound_node_tag" json:"tag"`
	Listen           string                    `gorm:"size:64" json:"listen"`
	Port             int                       `json:"port"`
	Protocol         JSON[model.ProtocolSpec]  `json:"protocol"`
	Transport        JSON[model.TransportSpec] `json:"transport"`
	Security         JSON[model.SecuritySpec]  `json:"security"`
	Sniffing         JSON[model.SniffSpec]     `json:"sniffing"`
	Extra            json.RawMessage           `gorm:"type:text" json:"extra,omitempty"`
	Enabled          bool                      `json:"enabled"`
	TemplateID       string                    `gorm:"size:32;index" json:"templateId,omitempty"`
	OverriddenFields JSON[[]string]            `json:"overriddenFields"`
	ServiceOnly      bool                      `json:"serviceOnly"`                // relay-only inbound, hidden from subscriptions
	OutboundTag      string                    `gorm:"size:64" json:"outboundTag"` // route all traffic of this inbound to an outbound
	Remark           string                    `gorm:"size:255" json:"remark"`
	CreatedAt        time.Time                 `json:"createdAt"`
	UpdatedAt        time.Time                 `json:"updatedAt"`
}

// User is a panel user with quota.
type User struct {
	ID           string         `gorm:"primaryKey;size:32" json:"id"`
	Username     string         `gorm:"size:64;uniqueIndex" json:"username"`
	UUID         string         `gorm:"size:36;uniqueIndex" json:"uuid"`
	Password     string         `gorm:"size:64" json:"password"`
	SSSeed       string         `gorm:"size:64" json:"-"`
	TrafficLimit int64          `json:"trafficLimit"` // bytes, 0 = unlimited
	UsedUp       int64          `json:"usedUp"`
	UsedDown     int64          `json:"usedDown"`
	ExpireAt     *time.Time     `json:"expireAt"`
	IPLimit      int            `json:"ipLimit"`
	Status       string         `gorm:"size:16;index" json:"status"` // active / limited / expired / disabled
	SubToken     string         `gorm:"size:64;uniqueIndex" json:"subToken"`
	ResetPolicy  string         `gorm:"size:16" json:"resetPolicy"` // none / monthly / cycle
	ResetDay     int            `json:"resetDay"`                   // day of month for monthly, days for cycle
	LastResetAt  *time.Time     `json:"lastResetAt"`
	OnlineAt     *time.Time     `json:"onlineAt"`
	BannedUntil  *time.Time     `json:"bannedUntil"`
	OnlineIPs    JSON[[]string] `json:"onlineIps"`
	IsService    bool           `gorm:"index" json:"isService"` // chain service account
	Remark       string         `gorm:"size:255" json:"remark"`
	GroupIDs     []string       `gorm:"-" json:"groupIds"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

// Used returns total used bytes.
func (u *User) Used() int64 { return u.UsedUp + u.UsedDown }

// ToModel converts to the core-facing user.
func (u *User) ToModel() model.User {
	return model.User{ID: u.ID, UUID: u.UUID, Password: u.Password, SSSeed: u.SSSeed}
}

// Group grants inbounds to users.
type Group struct {
	ID         string    `gorm:"primaryKey;size:32" json:"id"`
	Name       string    `gorm:"size:64;uniqueIndex" json:"name"`
	Remark     string    `gorm:"size:255" json:"remark"`
	InboundIDs []string  `gorm:"-" json:"inboundIds"`
	UserCount  int64     `gorm:"-" json:"userCount"`
	CreatedAt  time.Time `json:"createdAt"`
}

type GroupInbound struct {
	GroupID   string `gorm:"primaryKey;size:32"`
	InboundID string `gorm:"primaryKey;size:32;index"`
}

type UserGroup struct {
	UserID  string `gorm:"primaryKey;size:32"`
	GroupID string `gorm:"primaryKey;size:32;index"`
}

// TrafficHourly keeps hourly traffic per user and node (90 days).
type TrafficHourly struct {
	UserID string    `gorm:"primaryKey;size:32"`
	NodeID string    `gorm:"primaryKey;size:32"`
	Hour   time.Time `gorm:"primaryKey"`
	Up     int64
	Down   int64
}

// Certificate is a TLS certificate distributed to nodes.
type Certificate struct {
	ID        string     `gorm:"primaryKey;size:32" json:"id"`
	Domain    string     `gorm:"size:255;index" json:"domain"`
	Provider  string     `gorm:"size:32" json:"provider"` // acme / selfsigned / manual / cf-origin
	Cert      string     `gorm:"type:text" json:"cert"`
	Key       string     `gorm:"type:text" json:"-"` // sealed
	ExpireAt  *time.Time `json:"expireAt"`
	AutoRenew bool       `json:"autoRenew"`
	LastError string     `gorm:"size:2000" json:"lastError"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// ConfigRevision is one rendered NodeSpec; any revision can be rolled back to.
type ConfigRevision struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	NodeID    string    `gorm:"size:32;uniqueIndex:idx_rev_node" json:"nodeId"`
	Rev       int64     `gorm:"uniqueIndex:idx_rev_node" json:"rev"`
	Spec      string    `gorm:"type:text" json:"-"` // sealed JSON
	Reason    string    `gorm:"size:255" json:"reason"`
	Status    string    `gorm:"size:16" json:"status"` // pending / applied / failed
	Method    string    `gorm:"size:8" json:"method"`  // hot / reload
	Error     string    `gorm:"size:2000" json:"error"`
	CreatedAt time.Time `json:"createdAt"`
}

// Admin is a panel administrator.
type Admin struct {
	ID             string     `gorm:"primaryKey;size:32" json:"id"`
	Username       string     `gorm:"size:64;uniqueIndex" json:"username"`
	PasswordHash   string     `gorm:"size:100" json:"-"`
	TOTPSecret     string     `gorm:"size:255" json:"-"` // sealed
	TOTPEnabled    bool       `json:"totpEnabled"`
	FailedAttempts int        `json:"-"`
	LockedUntil    *time.Time `json:"-"`
	LastLoginAt    *time.Time `json:"lastLoginAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// AuditLog records every write operation.
type AuditLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Admin     string    `gorm:"size:64;index" json:"admin"`
	Action    string    `gorm:"size:64;index" json:"action"`
	Target    string    `gorm:"size:255" json:"target"`
	Detail    string    `gorm:"type:text" json:"detail"`
	IP        string    `gorm:"size:64" json:"ip"`
	CreatedAt time.Time `gorm:"index" json:"createdAt"`
}

// Endpoint is one way for clients to reach an inbound (direct / cdn / relay).
type Endpoint struct {
	ID           string    `gorm:"primaryKey;size:32" json:"id"`
	InboundID    string    `gorm:"size:32;index" json:"inboundId"`
	Kind         string    `gorm:"size:16" json:"kind"` // direct / cdn / relay
	Address      string    `gorm:"size:255" json:"address"`
	Port         int       `json:"port"`
	SNI          string    `gorm:"size:255" json:"sni"`
	Host         string    `gorm:"size:255" json:"host"`
	Path         string    `gorm:"size:255" json:"path"`
	CDNProfileID string    `gorm:"size:32;index" json:"cdnProfileId"`
	RelayChainID string    `gorm:"size:32;index" json:"relayChainId"`
	Remark       string    `gorm:"size:255" json:"remark"`
	SortOrder    int       `json:"sortOrder"`
	CreatedAt    time.Time `json:"createdAt"`
}

// CDNProfile is a reusable CDN configuration.
type CDNProfile struct {
	ID            string         `gorm:"primaryKey;size:32" json:"id"`
	Name          string         `gorm:"size:64;uniqueIndex" json:"name"`
	Provider      string         `gorm:"size:32" json:"provider"` // cloudflare / other
	OriginDomain  string         `gorm:"size:255" json:"originDomain"`
	TLSMode       string         `gorm:"size:32" json:"tlsMode"` // full_strict / full / flexible
	Ports         JSON[[]int]    `json:"ports"`
	PreferredIPs  JSON[[]string] `json:"preferredIps"`
	IPSourceURL   string         `gorm:"size:500" json:"ipSourceUrl"`
	IPTopN        int            `json:"ipTopN"`
	ExpandPerIP   bool           `json:"expandPerIp"`
	AutoDNS       bool           `json:"autoDns"`
	CaddyFronting bool           `json:"caddyFronting"`
	APIToken      string         `gorm:"size:500" json:"-"` // sealed
	HasToken      bool           `gorm:"-" json:"hasToken"`
	ZoneID        string         `gorm:"size:64" json:"zoneId"`
	LastRefreshAt *time.Time     `json:"lastRefreshAt"`
	LastError     string         `gorm:"size:2000" json:"lastError"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

// Outbound is a node egress (persisted model.Outbound).
type Outbound struct {
	ID               string             `gorm:"primaryKey;size:32" json:"id"`
	NodeID           string             `gorm:"size:32;index;uniqueIndex:idx_out_node_tag" json:"nodeId"`
	Tag              string             `gorm:"size:64;uniqueIndex:idx_out_node_tag" json:"tag"`
	Type             model.OutboundType `gorm:"size:16" json:"type"`
	Settings         json.RawMessage    `gorm:"type:text" json:"settings,omitempty"`
	NodeRefInboundID string             `gorm:"size:32;index" json:"nodeRefInboundId"`
	Detour           string             `gorm:"size:64" json:"detour"`
	ChainID          string             `gorm:"size:32;index" json:"chainId"` // managed by a chain
	ServiceUserID    string             `gorm:"size:32" json:"serviceUserId"` // node_ref credentials
	Remark           string             `gorm:"size:255" json:"remark"`
	CreatedAt        time.Time          `json:"createdAt"`
	UpdatedAt        time.Time          `json:"updatedAt"`
}

// ChainHop is one node in a relay chain. The last hop is the landing inbound.
type ChainHop struct {
	NodeID    string `json:"nodeId"`
	InboundID string `json:"inboundId,omitempty"` // proxy mode: relay inbound; last hop: landing inbound
	Port      int    `json:"port,omitempty"`      // l4 mode: listen port on the relay
}

// HopProbe is the latest latency measurement of one hop.
type HopProbe struct {
	NodeID    string `json:"nodeId"`
	LatencyMs int    `json:"latencyMs"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// Chain is an ingress relay path to a landing inbound.
type Chain struct {
	ID            string           `gorm:"primaryKey;size:32" json:"id"`
	Name          string           `gorm:"size:64;uniqueIndex" json:"name"`
	Mode          string           `gorm:"size:8" json:"mode"` // l4 / proxy
	Hops          JSON[[]ChainHop] `json:"hops"`
	ServiceUserID string           `gorm:"size:32" json:"serviceUserId"`
	Probe         JSON[[]HopProbe] `json:"probe"`
	ProbedAt      *time.Time       `json:"probedAt"`
	Remark        string           `gorm:"size:255" json:"remark"`
	CreatedAt     time.Time        `json:"createdAt"`
	UpdatedAt     time.Time        `json:"updatedAt"`
}

// RoutingRule is a persisted model.RouteRule.
type RoutingRule struct {
	ID          string                 `gorm:"primaryKey;size:32" json:"id"`
	NodeID      string                 `gorm:"size:32;index" json:"nodeId"`
	Match       JSON[model.RouteMatch] `json:"match"`
	OutboundTag string                 `gorm:"size:64" json:"outboundTag"`
	Priority    int                    `json:"priority"`
	ChainID     string                 `gorm:"size:32;index" json:"chainId"` // managed by a chain
	Remark      string                 `gorm:"size:255" json:"remark"`
	CreatedAt   time.Time              `json:"createdAt"`
}

// InboundTemplate generates inbounds on many nodes.
type InboundTemplate struct {
	ID          string               `gorm:"primaryKey;size:32" json:"id"`
	Name        string               `gorm:"size:64;uniqueIndex" json:"name"`
	Builtin     bool                 `json:"builtin"`
	Description string               `gorm:"size:255" json:"description"`
	Items       JSON[[]TemplateItem] `json:"items"`
	Groups      JSON[[]string]       `json:"groups"`
	BoundLabels JSON[[]string]       `json:"boundLabels"`
	CreatedAt   time.Time            `json:"createdAt"`
	UpdatedAt   time.Time            `json:"updatedAt"`
}

// TemplateItem describes one inbound a template generates.
type TemplateItem struct {
	Key        string              `json:"key"`  // stable id within the template
	Core       model.CoreType      `json:"core"` // concrete core or auto
	Protocol   model.ProtocolSpec  `json:"protocol"`
	Transport  model.TransportSpec `json:"transport"`
	Security   model.SecuritySpec  `json:"security"`
	Sniffing   model.SniffSpec     `json:"sniffing"`
	Port       PortPolicy          `json:"port"`
	TagPattern string              `json:"tagPattern"` // e.g. "{node.name}-{protocol}"
	Cert       string              `json:"cert"`       // acme / wildcard:<certId> / selfsigned / none
	Endpoints  []EndpointTemplate  `json:"endpoints"`
}

// PortPolicy picks ports per node.
type PortPolicy struct {
	Mode  string `json:"mode"` // fixed / range
	Fixed int    `json:"fixed,omitempty"`
	Min   int    `json:"min,omitempty"`
	Max   int    `json:"max,omitempty"`
}

// EndpointTemplate generates endpoints for each generated inbound.
type EndpointTemplate struct {
	Kind         string `json:"kind"` // direct / cdn / relay
	CDNProfileID string `json:"cdnProfileId,omitempty"`
	RelayChainID string `json:"relayChainId,omitempty"`
	Address      string `json:"address,omitempty"` // may use variables
	Port         int    `json:"port,omitempty"`
}

// Setting is a key/value system setting.
type Setting struct {
	Key   string `gorm:"primaryKey;size:64" json:"key"`
	Value string `gorm:"type:text" json:"value"`
}

// SubTemplate is an editable rule template per subscription format.
type SubTemplate struct {
	ID        string    `gorm:"primaryKey;size:32" json:"id"`
	Format    string    `gorm:"size:16;index" json:"format"` // clash / singbox / surge
	Name      string    `gorm:"size:64" json:"name"`
	Content   string    `gorm:"type:text" json:"content"`
	IsDefault bool      `json:"isDefault"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DNSProvider holds credentials for a DNS API used by domain bindings and DDNS.
type DNSProvider struct {
	ID        string    `gorm:"primaryKey;size:32" json:"id"`
	Name      string    `gorm:"size:64;uniqueIndex" json:"name"`
	Kind      string    `gorm:"size:16" json:"kind"` // cloudflare / webhook
	Token     string    `gorm:"size:1000" json:"-"`  // sealed API token (cloudflare)
	Endpoint  string    `gorm:"size:500" json:"endpoint"`
	HasToken  bool      `gorm:"-" json:"hasToken"`
	CreatedAt time.Time `json:"createdAt"`
}

// DomainBinding points a DNS name at a node's IP and keeps it updated (DDNS).
type DomainBinding struct {
	ID         string     `gorm:"primaryKey;size:32" json:"id"`
	NodeID     string     `gorm:"size:32;index" json:"nodeId"`
	ProviderID string     `gorm:"size:32;index" json:"providerId"`
	Domain     string     `gorm:"size:255;uniqueIndex" json:"domain"`
	Family     string     `gorm:"size:8" json:"family"` // ipv4 / ipv6 / dual
	Proxied    bool       `json:"proxied"`
	TTL        int        `json:"ttl"`
	Current    string     `gorm:"size:200" json:"current"` // last value written
	Status     string     `gorm:"size:16" json:"status"`   // pending / synced / error
	LastError  string     `gorm:"size:2000" json:"lastError"`
	SyncedAt   *time.Time `json:"syncedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// AllModels lists every table for migration.
func AllModels() []any {
	return []any{
		&Node{}, &Core{}, &Inbound{}, &User{}, &Group{}, &GroupInbound{}, &UserGroup{},
		&TrafficHourly{}, &Certificate{}, &ConfigRevision{}, &Admin{}, &AuditLog{},
		&Endpoint{}, &CDNProfile{}, &Outbound{}, &Chain{}, &RoutingRule{}, &InboundTemplate{},
		&Setting{}, &SubTemplate{}, &DNSProvider{}, &DomainBinding{},
	}
}
