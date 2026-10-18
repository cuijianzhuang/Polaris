package store

import (
	"encoding/json"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Node is one managed server running an agent.
type Node struct {
	ID           string           `gorm:"primaryKey;size:32" json:"id"`
	Name         string           `gorm:"size:64;uniqueIndex" json:"name"`
	Address      string           `gorm:"size:255" json:"address"` // public host/IP used in subscriptions
	Domain       string           `gorm:"size:255" json:"domain"`  // optional, for ACME and {node.domain}
	Region       string           `gorm:"size:64" json:"region"`
	Flag         string           `gorm:"size:8" json:"flag"` // ISO country code for emoji flags
	Lat          *float64         `json:"lat"`                // optional map position, degrees; both or neither
	Lng          *float64         `json:"lng"`
	Labels       JSON[[]string]   `json:"labels"`
	AgentVersion string           `gorm:"size:32" json:"agentVersion"`
	Arch         string           `gorm:"size:16" json:"arch"`         // agent GOARCH, "" until a new agent reports it
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
	DDNS         bool             `json:"ddns"`       // follow the agent-detected public IP
	NoFirewall   bool             `json:"noFirewall"` // don't let the agent manage host firewall ports
	PublicIPv4   string           `gorm:"size:64" json:"publicIpv4"`
	PublicIPv6   string           `gorm:"size:64" json:"publicIpv6"`
	IPChangedAt  *time.Time       `json:"ipChangedAt"`
	// Level is the tier a group needs to reach this node (0 = not tiered:
	// only explicit grants apply).
	Level int `json:"level"`
	// Server traffic: what the host's NICs moved this billing cycle, against
	// the provider's monthly allowance. NetMode decides what counts: "sum"
	// (up + down, default), "up", "down" or "max".
	NetLimit      int64      `json:"netLimit"` // bytes per cycle, 0 = unlimited
	NetMode       string     `gorm:"size:8" json:"netMode"`
	NetResetDay   int        `json:"netResetDay"` // day of month the cycle restarts, 0 = never
	NetWarnPct    int        `json:"netWarnPct"`  // alert at this share of the limit, 0 = 80
	NetOffset     int64      `json:"netOffset"`   // added to the counted total (calibration)
	NetRx         int64      `json:"netRx"`       // received this cycle
	NetTx         int64      `json:"netTx"`       // sent this cycle
	NetCycleStart *time.Time `json:"netCycleStart"`
	NetWarned     int        `json:"netWarned"` // 1 = threshold alert sent, 2 = over-limit alert sent
	NetLastRx     uint64     `json:"-"`         // last reported NIC counters
	NetLastTx     uint64     `json:"-"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
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
	RunningVersion    string         `gorm:"size:32" json:"runningVersion"` // version the agent reports running
	PrevVersion       string         `gorm:"size:32" json:"prevVersion"`    // version before the last switch (rollback target)
	NoUserTraffic     bool           `json:"noUserTraffic"`                 // core reports no per-user traffic (official sing-box)
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
	TemplateItemKey  string                    `gorm:"size:64" json:"templateItemKey,omitempty"` // which template item generated it
	OverriddenFields JSON[[]string]            `json:"overriddenFields"`
	ServiceOnly      bool                      `json:"serviceOnly"`                // relay-only inbound, hidden from subscriptions
	OutboundTag      string                    `gorm:"size:64" json:"outboundTag"` // route all traffic of this inbound to an outbound
	SpeedLimitMbps   int                       `json:"speedLimitMbps"`             // port bandwidth cap on the node (tc), 0 = none
	Remark           string                    `gorm:"size:255" json:"remark"`
	CreatedAt        time.Time                 `json:"createdAt"`
	UpdatedAt        time.Time                 `json:"updatedAt"`
}

// User is a panel user with quota.
type User struct {
	ID           string     `gorm:"primaryKey;size:32" json:"id"`
	Username     string     `gorm:"size:64;uniqueIndex" json:"username"`
	UUID         string     `gorm:"size:36;uniqueIndex" json:"uuid"`
	Password     string     `gorm:"size:64" json:"password"`
	SSSeed       string     `gorm:"size:64" json:"-"`
	TrafficLimit int64      `json:"trafficLimit"` // bytes, 0 = unlimited
	UsedUp       int64      `json:"usedUp"`
	UsedDown     int64      `json:"usedDown"`
	ExpireAt     *time.Time `json:"expireAt"`
	// ExpireAfterDays starts the validity period on first use (ExpireAt is
	// set when traffic is first seen).
	ExpireAfterDays int    `json:"expireAfterDays"`
	IPLimit         int    `json:"ipLimit"`
	Status          string `gorm:"size:16;index" json:"status"` // active / limited / expired / disabled
	SubToken        string `gorm:"size:64;uniqueIndex" json:"subToken"`
	ResetPolicy     string `gorm:"size:16" json:"resetPolicy"` // none / daily / weekly / monthly / cycle
	ResetDay        int    `json:"resetDay"`                   // weekday 1-7 (weekly), day of month (monthly), days (cycle)
	// RenewDays auto-renews an expired account: expiry moves on by this many
	// days and traffic starts over, at most RenewMax times (0 = no cap).
	RenewDays  int `json:"renewDays"`
	RenewMax   int `json:"renewMax"`
	RenewCount int `json:"renewCount"`
	// DeviceLimit caps the client devices (HWID sent with subscription
	// requests) that may fetch the subscription; 0 = unlimited.
	DeviceLimit int `json:"deviceLimit"`
	// SpeedUpMbps / SpeedDownMbps cap the user's bandwidth on each node
	// (0 = use the groups' plan; a user in no limited group is unlimited).
	SpeedUpMbps   int            `json:"speedUpMbps"`
	SpeedDownMbps int            `json:"speedDownMbps"`
	LastSubAt     *time.Time     `json:"lastSubAt"`
	LastSubUA     string         `gorm:"size:255" json:"lastSubUa"`
	LastResetAt   *time.Time     `json:"lastResetAt"`
	OnlineAt      *time.Time     `json:"onlineAt"`
	BannedUntil   *time.Time     `json:"bannedUntil"`
	OnlineIPs     JSON[[]string] `json:"onlineIps"`
	// OnlineDevices is what is connected right now, filled per request from
	// the panel's live reports (never stored).
	OnlineDevices []OnlineDevice `gorm:"-" json:"onlineDevices"`
	// PlanID is the user's current plan (a Group with Plan set), "" = none.
	PlanID       string     `gorm:"size:32;index" json:"planId"`
	IsService    bool       `gorm:"index" json:"isService"`  // chain service account
	TelegramID   int64      `gorm:"index" json:"telegramId"` // bound Telegram chat
	WarnedUsage  bool       `json:"-"`                       // 80% traffic warning sent this cycle
	WarnedExpire *time.Time `json:"-"`
	Remark       string     `gorm:"size:255" json:"remark"`
	GroupIDs     []string   `gorm:"-" json:"groupIds"`
	NodeIDs      []string   `gorm:"-" json:"nodeIds"`    // direct node bindings
	InboundIDs   []string   `gorm:"-" json:"inboundIds"` // direct inbound bindings
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Used returns total used bytes.
func (u *User) Used() int64 { return u.UsedUp + u.UsedDown }

// ToModel converts to the core-facing user.
func (u *User) ToModel() model.User {
	return model.User{ID: u.ID, UUID: u.UUID, Password: u.Password, SSSeed: u.SSSeed}
}

// Group grants inbounds to users.
type Group struct {
	ID        string `gorm:"primaryKey;size:32" json:"id"`
	Name      string `gorm:"size:64;uniqueIndex" json:"name"`
	Remark    string `gorm:"size:255" json:"remark"`
	IsDefault bool   `json:"isDefault"` // new users and inbounds join it unless told otherwise
	// Level opens every node whose Level is 1..Level to the group's users
	// (0 = no tier access); NodeIDs binds whole nodes regardless of level.
	Level int `json:"level"`
	// SpeedUpMbps / SpeedDownMbps are the group's plan: members without
	// their own limit get the highest plan among their groups (0 = none).
	SpeedUpMbps   int `json:"speedUpMbps"`
	SpeedDownMbps int `json:"speedDownMbps"`
	// A plan (Plan = true) is a group sold as a package. Applying it to a
	// user makes it the user's only plan group and sets the limits below;
	// the group's level, nodes, inbounds and speed decide what it reaches.
	Plan         bool      `gorm:"index" json:"plan"`
	TrafficLimit int64     `json:"trafficLimit"` // bytes, 0 = unlimited
	Days         int       `json:"days"`         // validity, 0 = no expiry
	ResetPolicy  string    `gorm:"size:16" json:"resetPolicy"`
	ResetDay     int       `json:"resetDay"`
	DeviceLimit  int       `json:"deviceLimit"`
	IPLimit      int       `json:"ipLimit"`
	Price        string    `gorm:"size:32" json:"price"`      // shown only, e.g. "¥15 / 月"
	FallbackID   string    `gorm:"size:32" json:"fallbackId"` // plan applied on expiry; "" = the account expires
	Sort         int       `json:"sort"`
	NodeIDs      []string  `gorm:"-" json:"nodeIds"`
	InboundIDs   []string  `gorm:"-" json:"inboundIds"`
	UserCount    int64     `gorm:"-" json:"userCount"`
	CreatedAt    time.Time `json:"createdAt"`
}

// NodeTrafficDaily is a node's NIC traffic per day (panel local time).
type NodeTrafficDaily struct {
	NodeID string `gorm:"primaryKey;size:32" json:"nodeId"`
	Day    string `gorm:"primaryKey;size:10" json:"day"`
	Rx     int64  `json:"rx"`
	Tx     int64  `json:"tx"`
}

// NodeCost is what a node's server costs and when it renews. Cycle: month,
// quarter, halfyear, year or once.
type NodeCost struct {
	NodeID    string     `gorm:"primaryKey;size:32" json:"nodeId"`
	Provider  string     `gorm:"size:64" json:"provider"`
	Amount    float64    `json:"amount"`
	Currency  string     `gorm:"size:8" json:"currency"`
	Cycle     string     `gorm:"size:16" json:"cycle"`
	ExpireAt  *time.Time `json:"expireAt"`
	AutoRenew bool       `json:"autoRenew"`
	Note      string     `gorm:"size:255" json:"note"`
	WarnedFor string     `gorm:"size:10" json:"-"` // expiry date the reminder was sent for
	UpdatedAt time.Time  `json:"updatedAt"`
}

// GroupNode gives a group's users every inbound on a node.
type GroupNode struct {
	GroupID string `gorm:"primaryKey;size:32"`
	NodeID  string `gorm:"primaryKey;size:32;index"`
}

type GroupInbound struct {
	GroupID   string `gorm:"primaryKey;size:32"`
	InboundID string `gorm:"primaryKey;size:32;index"`
}

type UserGroup struct {
	UserID  string `gorm:"primaryKey;size:32"`
	GroupID string `gorm:"primaryKey;size:32;index"`
}

// UserAccess binds a user directly to a whole node (every inbound on it,
// including ones added later) or to one inbound, alongside group grants.
type UserAccess struct {
	UserID   string `gorm:"primaryKey;size:32"`
	Kind     string `gorm:"primaryKey;size:8"` // node / inbound
	TargetID string `gorm:"primaryKey;size:32;index"`
}

// GrantsSQL selects (user_id, inbound_id) for every way a user can be
// authorised on an inbound: group membership (its inbounds, its whole nodes,
// or nodes within its level), a direct inbound binding, or a node binding.
const GrantsSQL = `SELECT ug.user_id AS user_id, gi.inbound_id AS inbound_id FROM user_groups ug JOIN group_inbounds gi ON gi.group_id = ug.group_id
UNION SELECT ua.user_id, ua.target_id FROM user_accesses ua WHERE ua.kind = 'inbound'
UNION SELECT ua.user_id, i.id FROM user_accesses ua JOIN inbounds i ON i.node_id = ua.target_id WHERE ua.kind = 'node'
UNION SELECT ug.user_id, i.id FROM user_groups ug JOIN group_nodes gn ON gn.group_id = ug.group_id JOIN inbounds i ON i.node_id = gn.node_id
UNION SELECT ug.user_id, i.id FROM user_groups ug JOIN "groups" g ON g.id = ug.group_id JOIN nodes n ON n.level > 0 AND n.level <= g.level JOIN inbounds i ON i.node_id = n.id`

// TrafficHourly keeps hourly traffic per user and node (90 days).
type TrafficHourly struct {
	UserID string    `gorm:"primaryKey;size:32"`
	NodeID string    `gorm:"primaryKey;size:32"`
	Hour   time.Time `gorm:"primaryKey"`
	Up     int64
	Down   int64
	// Older rows have no raw/account evidence. RecordedUp/Down track only
	// the portion captured after this schema, including mixed upgrade hours.
	RawRecorded  bool    `gorm:"not null;default:false"`
	RawUp        int64   `gorm:"not null;default:0"`
	RawDown      int64   `gorm:"not null;default:0"`
	RecordedUp   int64   `gorm:"not null;default:0"`
	RecordedDown int64   `gorm:"not null;default:0"`
	AccountUp    int64   `gorm:"not null;default:0"`
	AccountDown  int64   `gorm:"not null;default:0"`
	RateMin      float64 `gorm:"not null;default:0"`
	RateMax      float64 `gorm:"not null;default:0"`
}

// Certificate is a TLS certificate distributed to nodes.
type Certificate struct {
	ID        string     `gorm:"primaryKey;size:32" json:"id"`
	Domain    string     `gorm:"size:255;index" json:"domain"`
	Provider  string     `gorm:"size:32" json:"provider"` // acme / selfsigned / manual / cf-origin
	Challenge string     `gorm:"size:8" json:"challenge"` // acme: dns (Cloudflare) / http (node port 80)
	NodeID    string     `gorm:"size:32" json:"nodeId"`   // acme http: node that answers the challenge
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
	TOTPPending    string     `gorm:"size:255" json:"-"` // sealed; becomes TOTPSecret once a code verifies
	TokenVersion   int        `json:"-"`                 // bumped to revoke every issued session
	FailedAttempts int        `json:"-"`
	LockedUntil    *time.Time `json:"-"`
	LastLoginAt    *time.Time `json:"lastLoginAt"`
	GitHubID       int64      `gorm:"index" json:"-"`             // bound GitHub account (sign-in)
	GitHubLogin    string     `gorm:"size:64" json:"githubLogin"` // its username, for display
	// RecoveryCodes are SHA-256 hashes of one-time codes that stand in for
	// a TOTP code when the authenticator is lost; each works once.
	RecoveryCodes JSON[[]string] `json:"-"`
	CreatedAt     time.Time      `json:"createdAt"`
}

// AdminSession is one signed-in browser (the JWT carries its id): listed in
// the settings, revocable one by one. Sudo marks a recent re-verification
// that unlocks sensitive actions for a few minutes.
type AdminSession struct {
	ID        string     `gorm:"primaryKey;size:32" json:"id"`
	AdminID   string     `gorm:"size:40;index" json:"-"` // admin id, or "tg:" + Telegram admin id
	Username  string     `gorm:"size:64" json:"username"`
	Via       string     `gorm:"size:16" json:"via"` // password / github / telegram
	IP        string     `gorm:"size:64;index" json:"ip"`
	UserAgent string     `gorm:"size:255" json:"userAgent"`
	CreatedAt time.Time  `json:"createdAt"`
	LastSeen  time.Time  `json:"lastSeen"`
	ExpiresAt time.Time  `json:"expiresAt"`
	SudoUntil *time.Time `json:"-"`
	Revoked   bool       `json:"revoked"`
}

// SSOProvider is a single sign-on provider for the admin login page: an
// OpenID Connect issuer (endpoints discovered) or a plain OAuth 2.0 server.
type SSOProvider struct {
	ID           string    `gorm:"primaryKey;size:32" json:"id"`
	Name         string    `gorm:"size:64" json:"name"` // button label
	Kind         string    `gorm:"size:16" json:"kind"` // oidc / oauth2
	Issuer       string    `gorm:"size:255" json:"issuer"`
	AuthURL      string    `gorm:"size:255" json:"authUrl"`
	TokenURL     string    `gorm:"size:255" json:"tokenUrl"`
	UserInfoURL  string    `gorm:"size:255" json:"userInfoUrl"`
	ClientID     string    `gorm:"size:255" json:"clientId"`
	ClientSecret string    `gorm:"size:512" json:"-"` // sealed
	Scopes       string    `gorm:"size:255" json:"scopes"`
	SubjectField string    `gorm:"size:64" json:"subjectField"` // userinfo field identifying the account
	LoginField   string    `gorm:"size:64" json:"loginField"`   // userinfo field shown as the account name
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
}

// SSOBinding ties an external account to a panel admin.
type SSOBinding struct {
	ProviderID string    `gorm:"primaryKey;size:32" json:"providerId"`
	Subject    string    `gorm:"primaryKey;size:255" json:"-"`
	AdminID    string    `gorm:"size:32;index" json:"-"`
	Login      string    `gorm:"size:255" json:"login"`
	CreatedAt  time.Time `json:"createdAt"`
}

// InboundHealth is the latest reachability probe of an inbound from the
// panel (TCP connect, plus a TLS handshake for TLS / REALITY).
type InboundHealth struct {
	InboundID string    `gorm:"primaryKey;size:32" json:"-"`
	OK        bool      `json:"ok"`
	LatencyMs int       `json:"latencyMs"`
	Error     string    `gorm:"size:255" json:"error"`
	Fails     int       `json:"fails"` // consecutive failures
	Alerted   bool      `json:"-"`
	Since     time.Time `json:"since"` // when the current state began
	CheckedAt time.Time `json:"checkedAt"`
}

// LoginBan tracks failed sign-ins per client IP; past the threshold the IP
// is banned, for longer each time it happens again.
type LoginBan struct {
	IP        string     `gorm:"primaryKey;size:64" json:"ip"`
	Failures  int        `json:"failures"` // in the current window
	WindowAt  time.Time  `json:"-"`
	Bans      int        `json:"bans"` // times banned so far
	Until     *time.Time `json:"until"`
	Reason    string     `gorm:"size:255" json:"reason"`
	UpdatedAt time.Time  `json:"updatedAt"`
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

// ForwardRemote is one target of a standalone forward rule.
type ForwardRemote struct {
	Addr    string `json:"addr"` // host:port
	Weight  int    `json:"weight,omitempty"`
	Enabled bool   `json:"enabled"`
}

// ForwardRule is a standalone L4 port forward on a node (realm).
type ForwardRule struct {
	ID         string                `gorm:"primaryKey;size:32" json:"id"`
	NodeID     string                `gorm:"size:32;index" json:"nodeId"`
	Engine     string                `gorm:"size:8" json:"engine"` // "" realm / gost
	Name       string                `gorm:"size:64" json:"name"`
	ListenIP   string                `gorm:"size:64" json:"listenIp"`
	ListenPort int                   `json:"listenPort"`
	Network    string                `gorm:"size:16" json:"network"` // tcp / udp / tcp,udp
	Remotes    JSON[[]ForwardRemote] `json:"remotes"`
	Balance    string                `gorm:"size:16" json:"balance"` // "" (first only, extras as fallback list) / roundrobin / iphash
	Failover   bool                  `json:"failover"`               // drop unreachable remotes (probed from the node)
	Through    string                `gorm:"size:64" json:"through"`
	// Tunnel between a relay realm (role client) and a landing realm (role
	// server): ws / tls / wss with Host / Path / SNI.
	TunnelRole  string                `gorm:"size:8" json:"tunnelRole"` // "" / client / server
	TunnelType  string                `gorm:"size:8" json:"tunnelType"` // ws / tls / wss
	TunnelHost  string                `gorm:"size:255" json:"tunnelHost"`
	TunnelPath  string                `gorm:"size:255" json:"tunnelPath"`
	TunnelSNI   string                `gorm:"size:255" json:"tunnelSni"`
	TunnelAuth  string                `gorm:"size:128" json:"tunnelAuth"` // gost relay password
	RateIn      int                   `json:"rateIn"`                     // gost: KB/s, 0 = unlimited
	RateOut     int                   `json:"rateOut"`
	MaxConns    int                   `json:"maxConns"` // gost: concurrent connections
	SendProxy   int                   `json:"sendProxy"`
	AcceptProxy bool                  `json:"acceptProxy"`
	Enabled     bool                  `json:"enabled"`
	Health      JSON[map[string]bool] `json:"health"` // remote -> down (failover)
	Remark      string                `gorm:"size:255" json:"remark"`
	CreatedAt   time.Time             `json:"createdAt"`
	UpdatedAt   time.Time             `json:"updatedAt"`
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

// SubSource is an upstream subscription URL or a list of manual node links,
// merged into subscription profiles by the conversion engine.
type SubSource struct {
	ID         string           `gorm:"primaryKey;size:32" json:"id"`
	Name       string           `gorm:"size:64" json:"name"`
	Kind       string           `gorm:"size:8" json:"kind"` // url / manual
	URL        string           `gorm:"size:2000" json:"url"`
	Content    string           `gorm:"type:text" json:"content"` // manual: share links / YAML / JSON
	UserAgent  string           `gorm:"size:200" json:"userAgent"`
	Group      string           `gorm:"size:64" json:"group"`
	Enabled    bool             `json:"enabled"`
	RefreshMin int              `json:"refreshMin"`         // auto refresh interval (minutes), 0 = manual
	Cache      string           `gorm:"type:text" json:"-"` // last parsed nodes (JSON)
	NodeCount  int              `json:"nodeCount"`
	UserInfo   JSON[SubTraffic] `json:"userInfo"`
	FetchedAt  *time.Time       `json:"fetchedAt"`
	LastError  string           `gorm:"size:2000" json:"lastError"`
	SortOrder  int              `json:"sortOrder"`
	Remark     string           `gorm:"size:255" json:"remark"`
	CreatedAt  time.Time        `json:"createdAt"`
}

// SubTraffic mirrors the subscription-userinfo header.
type SubTraffic struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
	Expire   int64 `json:"expire"` // unix seconds, 0 = none
}

// SubProfile combines panel nodes and sources into one published subscription.
type SubProfile struct {
	ID            string                  `gorm:"primaryKey;size:32" json:"id"`
	Name          string                  `gorm:"size:64;uniqueIndex" json:"name"`
	Token         string                  `gorm:"size:64;uniqueIndex" json:"token"`
	Enabled       bool                    `json:"enabled"`
	SourceIDs     JSON[[]string]          `json:"sourceIds"`
	PanelUserID   string                  `gorm:"size:32" json:"panelUserId"` // include this user's panel nodes
	Operators     JSON[[]sub.Operator]    `json:"operators"`                  // empty = global default chain
	UseGlobalOps  bool                    `json:"useGlobalOps"`
	Templates     JSON[map[string]string] `json:"templates"`                // format -> sub template id
	Converter     string                  `gorm:"size:16" json:"converter"` // "" = global setting, builtin, external
	Backend       string                  `gorm:"size:500" json:"backend"`  // external backend override
	BackendConfig string                  `gorm:"size:1000" json:"backendConfig"`
	Scheme        string                  `gorm:"size:1000" json:"scheme"` // rule scheme: "" = global, "none", preset id or ini URL
	SkipCert      *bool                   `json:"skipCert"`                // nil = global setting
	ForceUDP      *bool                   `json:"forceUdp"`
	Downloads     int64                   `json:"downloads"`
	Remark        string                  `gorm:"size:255" json:"remark"`
	CreatedAt     time.Time               `json:"createdAt"`
	UpdatedAt     time.Time               `json:"updatedAt"`
}

// TGAdmin is a Telegram chat allowed to administer the panel. Owners see
// everything; operators only the nodes they are bound to.
type TGAdmin struct {
	ID        string         `gorm:"primaryKey;size:32" json:"id"`
	ChatID    int64          `gorm:"uniqueIndex" json:"chatId"`
	Name      string         `gorm:"size:64" json:"name"`
	Role      string         `gorm:"size:16" json:"role"` // owner / operator
	NodeIDs   JSON[[]string] `json:"nodeIds"`
	Notify    bool           `json:"notify"`
	CreatedAt time.Time      `json:"createdAt"`
}

// AllModels lists every table for migration.
func AllModels() []any {
	return []any{
		&Node{}, &Core{}, &Inbound{}, &User{}, &Group{}, &GroupInbound{}, &UserGroup{}, &UserAccess{},
		&TrafficHourly{}, &Certificate{}, &ConfigRevision{}, &Admin{}, &AuditLog{},
		&Endpoint{}, &CDNProfile{}, &Outbound{}, &Chain{}, &RoutingRule{}, &InboundTemplate{},
		&Setting{}, &SubTemplate{}, &DNSProvider{}, &DomainBinding{}, &SubSource{}, &SubProfile{}, &TGAdmin{},
		&UserDevice{}, &TagTraffic{}, &ForwardRule{}, &UserNodeQuota{},
		&TrafficDaily{}, &TagTrafficDaily{}, &NodeMetric{}, &AdminSession{}, &LoginBan{}, &InboundHealth{}, &SSOProvider{}, &SSOBinding{}, &GroupNode{}, &NodeTrafficDaily{}, &NodeCost{},
		&DNSConfig{}, &RuleSet{}, &APIToken{}, &IdempotencyKey{}, &Webhook{}, &WebhookDelivery{}, &Plugin{},
	}
}

// APIToken lets scripts and plugins call the API without a login session.
// Only the SHA-256 of the token is stored; Scopes limit what it may call.
type APIToken struct {
	ID         string         `gorm:"primaryKey;size:32" json:"id"`
	Name       string         `gorm:"size:64" json:"name"`
	PluginID   string         `gorm:"size:64;index" json:"pluginId,omitempty"` // set for a plugin's own token
	Hash       string         `gorm:"size:64;uniqueIndex" json:"-"`
	Prefix     string         `gorm:"size:16" json:"prefix"` // first characters, to recognise a token
	Scopes     JSON[[]string] `json:"scopes"`
	CreatedBy  string         `gorm:"size:64" json:"createdBy"`
	CreatedAt  time.Time      `json:"createdAt"`
	LastUsedAt *time.Time     `json:"lastUsedAt"`
	LastIP     string         `gorm:"size:64" json:"lastIp"`
	ExpiresAt  *time.Time     `json:"expiresAt"`
}

// Plugin is an installed third-party plugin: an outside service described
// by its manifest, with its own API token and event webhook.
type Plugin struct {
	ID         string     `gorm:"primaryKey;size:64" json:"id"` // the manifest id
	Name       string     `gorm:"size:64" json:"name"`
	Version    string     `gorm:"size:32" json:"version"`
	BaseURL    string     `gorm:"size:1024" json:"baseUrl"`
	Manifest   string     `json:"-"`                        // the manifest as fetched (JSON)
	Settings   string     `json:"-"`                        // JSON object; secret values sealed
	Secret     string     `gorm:"size:255" json:"-"`        // sealed; signs calls to the plugin and page tokens
	TokenID    string     `gorm:"size:32" json:"tokenId"`   // its API token
	WebhookID  string     `gorm:"size:32" json:"webhookId"` // its event webhook
	Enabled    bool       `json:"enabled"`
	Status     string     `gorm:"size:16" json:"status"` // online / offline / manual (handshake not done)
	LastSeenAt *time.Time `json:"lastSeenAt"`
	LastError  string     `gorm:"size:512" json:"lastError"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// Webhook sends events to an outside URL: signed JSON for plugins, or a
// text message for chat tools (discord, slack, wecom, dingtalk, feishu).
type Webhook struct {
	ID         string         `gorm:"primaryKey;size:32" json:"id"`
	Name       string         `gorm:"size:64" json:"name"`
	URL        string         `gorm:"size:1024" json:"url"`
	Format     string         `gorm:"size:16" json:"format"` // json / discord / slack / wecom / dingtalk / feishu
	Secret     string         `gorm:"size:255" json:"-"`     // sealed; JSON signing key or the robot's signing secret
	Events     JSON[[]string] `json:"events"`                // empty = every event; "user.*" = a prefix
	Enabled    bool           `json:"enabled"`
	PluginID   string         `gorm:"size:64;index" json:"pluginId,omitempty"`
	LastStatus string         `gorm:"size:16" json:"lastStatus"`
	LastAt     *time.Time     `json:"lastAt"`
	CreatedAt  time.Time      `json:"createdAt"`
}

// WebhookDelivery is one event sent (or to be sent) to one webhook.
type WebhookDelivery struct {
	ID          string     `gorm:"primaryKey;size:32" json:"id"`
	WebhookID   string     `gorm:"size:32;index" json:"webhookId"`
	EventID     string     `gorm:"size:40" json:"eventId"`
	Type        string     `gorm:"size:64" json:"type"`
	Payload     string     `json:"payload"`
	Status      string     `gorm:"size:16;index" json:"status"` // pending / sending / success / failed
	Attempts    int        `json:"attempts"`
	NextAt      time.Time  `gorm:"index" json:"nextAt"`
	LastCode    int        `json:"lastCode"`
	LastError   string     `gorm:"size:512" json:"lastError"`
	LastAt      *time.Time `json:"lastAt"`
	DeliveredAt *time.Time `json:"deliveredAt"`
	CreatedAt   time.Time  `gorm:"index" json:"createdAt"`
}

// IdempotencyKey remembers the answer to a write sent with an
// Idempotency-Key header, so a retried call (a payment callback delivered
// twice) is not applied twice. User-command changes and their responses are
// committed in the same transaction. Records are kept for a day.
type IdempotencyKey struct {
	Key         string    `gorm:"primaryKey;size:200"`         // digest of stable caller, method, resource and header
	RequestHash string    `gorm:"size:64;not null;default:''"` // normalized query and request body
	Status      int       `json:"status"`
	Body        []byte    `json:"-"`
	CreatedAt   time.Time `gorm:"index"`
}

// TrafficDaily is a user's traffic on a node per day (panel local time). It
// outlives the hourly rows, which expire after 90 days.
type TrafficDaily struct {
	UserID string `gorm:"primaryKey;size:32"`
	NodeID string `gorm:"primaryKey;size:32"`
	Day    string `gorm:"primaryKey;size:10;index"` // YYYY-MM-DD
	Up     int64
	Down   int64
}

// TagTrafficDaily is an inbound's or outbound's traffic per day (raw bytes).
type TagTrafficDaily struct {
	NodeID string `gorm:"primaryKey;size:32"`
	Kind   string `gorm:"primaryKey;size:16"`
	Tag    string `gorm:"primaryKey;size:128"`
	Day    string `gorm:"primaryKey;size:10;index"`
	Up     int64
	Down   int64
}

// NodeMetric is a node's resource usage averaged over a 5-minute slot.
type NodeMetric struct {
	NodeID string    `gorm:"primaryKey;size:32" json:"-"`
	At     time.Time `gorm:"primaryKey" json:"t"`
	CPU    float64   `json:"cpu"`
	Mem    float64   `json:"mem"`
	RxBps  uint64    `json:"rx"`
	TxBps  uint64    `json:"tx"`
	Load1  float64   `json:"load"`
	N      int       `json:"-"` // samples averaged so far
}

// UserNodeQuota is a user's traffic on one node beyond the account quota:
// a node-only traffic pack (used before the account quota, one-off: it does
// not refill on a traffic reset) and a per-period cap on the node. Bytes are
// counted after the node's multiplier.
type UserNodeQuota struct {
	UserID    string    `gorm:"primaryKey;size:32" json:"userId"`
	NodeID    string    `gorm:"primaryKey;size:32;index" json:"nodeId"`
	Extra     int64     `json:"extra"`     // traffic pack size
	ExtraUsed int64     `json:"extraUsed"` // drawn from the pack so far
	Limit     int64     `json:"limit"`     // cap on this node per period, 0 = none
	Used      int64     `json:"used"`      // traffic on this node this period
	UpdatedAt time.Time `json:"updatedAt"`
}

// PackLeft is what remains of the traffic pack.
func (q UserNodeQuota) PackLeft() int64 { return max(q.Extra-q.ExtraUsed, 0) }

// Capped reports whether the per-node cap is reached.
func (q UserNodeQuota) Capped() bool { return q.Limit > 0 && q.Used >= q.Limit }

// OnlineDevice is one source IP a user is connected from, with the nodes it
// reached and, when the same IP fetched the subscription, the client's model.
type OnlineDevice struct {
	IP    string    `json:"ip"`
	Nodes []string  `json:"nodes"`
	Seen  time.Time `json:"seen"`
	Model string    `json:"model,omitempty"`
	OS    string    `json:"os,omitempty"`
}

// UserDevice is a client device (HWID) that fetched a user's subscription.
type UserDevice struct {
	ID        string    `gorm:"primaryKey;size:32" json:"id"`
	UserID    string    `gorm:"size:32;uniqueIndex:idx_user_hwid" json:"userId"`
	HWID      string    `gorm:"column:hwid;size:128;uniqueIndex:idx_user_hwid" json:"hwid"`
	OS        string    `gorm:"size:64" json:"os"`
	Model     string    `gorm:"size:128" json:"model"`
	UserAgent string    `gorm:"size:255" json:"userAgent"`
	IP        string    `gorm:"size:64" json:"ip"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// TagTraffic accumulates the bytes an inbound or outbound of a node carried
// since its last reset (Kind "inbound" / "outbound").
type TagTraffic struct {
	NodeID    string    `gorm:"size:32;uniqueIndex:idx_tag_traffic" json:"nodeId"`
	Kind      string    `gorm:"size:16;uniqueIndex:idx_tag_traffic" json:"kind"`
	Tag       string    `gorm:"size:128;uniqueIndex:idx_tag_traffic" json:"tag"`
	Up        int64     `json:"up"`
	Down      int64     `json:"down"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DNSConfig is the resolver config for every node (Scope "*") or one node.
// Mode: global "on" / "off"; per node "inherit" / "custom" / "off".
type DNSConfig struct {
	Scope     string              `gorm:"primaryKey;size:32" json:"scope"`
	Mode      string              `gorm:"size:16" json:"mode"`
	Spec      JSON[model.DNSSpec] `json:"spec"`
	UpdatedAt time.Time           `json:"updatedAt"`
}

// RuleSet is a named list of domains and IP ranges that routing rules and DNS
// servers refer to by tag. With SourceURL the panel refreshes the entries
// from a remote list daily and every node gets the result.
type RuleSet struct {
	ID            string         `gorm:"primaryKey;size:32" json:"id"`
	Tag           string         `gorm:"size:64;uniqueIndex" json:"tag"`
	Name          string         `gorm:"size:64" json:"name"`
	Remark        string         `gorm:"size:255" json:"remark"`
	SourceURL     string         `gorm:"size:512" json:"sourceUrl"`
	Domain        JSON[[]string] `json:"domain"`
	DomainSuffix  JSON[[]string] `json:"domainSuffix"`
	DomainKeyword JSON[[]string] `json:"domainKeyword"`
	IPCIDR        JSON[[]string] `json:"ipCidr"`
	FetchedAt     *time.Time     `json:"fetchedAt"`
	FetchError    string         `gorm:"size:500" json:"fetchError"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}
