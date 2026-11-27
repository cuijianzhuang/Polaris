package panel

import (
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Share subscriptions (design share-subscriptions): a user may hand out
// copies of their subscription, each with its own token, core credentials,
// traffic limit and expiry. Batch 1 ships the entity, the portal, the
// dual-path subscription resolution and the node-side shadow identities;
// per-share metering lands in batch 2 — until then settle strips the share
// suffix so share traffic counts toward the main account exactly like the
// main subscription's own traffic (nothing lost, nothing double-counted).

// shareSubMaxSetting is the global per-user share cap (D6): default 5,
// 0 closes share creation (existing shares stay valid until their own
// expiry, per the design).
const shareSubMaxSetting = "share_sub_max_per_user"

func (s *Server) shareSubMax() int {
	n, err := strconv.Atoi(strings.TrimSpace(s.st.GetSetting(shareSubMaxSetting, "5")))
	if err != nil || n < 0 {
		return 5
	}
	return n
}

// shareCreationOpen reports whether new shares may be created at all.
func (s *Server) shareCreationOpen() bool { return s.shareSubMax() > 0 }

// shareIdentity is the core-side identity of a share: the owning user's id
// plus the share id (D4). Cores and the agent treat it as an opaque string.
func shareIdentity(userID, shareID string) string { return userID + "#" + shareID }

// mainIdentity strips a share suffix from a core-side identity, returning
// the owning user's id. Plain user ids pass through unchanged.
func mainIdentity(id string) string {
	if i := strings.IndexByte(id, '#'); i >= 0 {
		return id[:i]
	}
	return id
}

// shareServed reports whether a share currently delivers nodes and is
// pushed to cores: enabled, not expired, not over its own limit. The main
// account's state is checked separately by the callers (a stopped main
// account takes every share down with it, D5).
func shareServed(sh *store.ShareSubscription, now time.Time) bool {
	return sh.Enabled && !sh.Expired(now) && !sh.Capped()
}

// shareExpiry returns the earlier of the share's and the main account's
// expiry (D5: whichever comes first stops the share).
func shareExpiry(u *store.User, sh *store.ShareSubscription) *time.Time {
	if sh.ExpireAt == nil {
		return u.ExpireAt
	}
	if u.ExpireAt != nil && u.ExpireAt.Before(*sh.ExpireAt) {
		return u.ExpireAt
	}
	return sh.ExpireAt
}

// shadowUser returns the user as a share sees it: the main account's
// grants, status and limits, but the share's own core credentials.
func shadowUser(u *store.User, sh *store.ShareSubscription) store.User {
	out := *u
	out.UUID, out.Password, out.SSSeed = sh.CredUUID, sh.CredPassword, sh.CredSSSeed
	return out
}

// newShareCredentials mints a fresh credential set for a share, the same
// way main credentials are minted (D2).
func newShareCredentials() (uuid, password, ssSeed string) {
	return pki.UUID(), pki.Password(20), pki.Token(24)
}

// servedShares loads the shares of the given users that are currently
// served, grouped by user id. One query for any number of users.
func (s *Server) servedShares(userIDs []string, now time.Time) (map[string][]store.ShareSubscription, error) {
	out := map[string][]store.ShareSubscription{}
	if len(userIDs) == 0 {
		return out, nil
	}
	var rows []store.ShareSubscription
	if err := s.st.DB.Where("user_id IN ? AND enabled = ?", userIDs, true).Find(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		if shareServed(&rows[i], now) {
			out[rows[i].UserID] = append(out[rows[i].UserID], rows[i])
		}
	}
	return out, nil
}

// touchShareUsed stamps a share's last activity, at most once an hour per
// share per instance (D9): the time is an operational hint, not a ledger.
func (s *Server) touchShareUsed(sh *store.ShareSubscription) {
	now := time.Now().UTC()
	s.shareMu.Lock()
	if s.shareTouched == nil {
		s.shareTouched = map[string]time.Time{}
	}
	if last, ok := s.shareTouched[sh.ID]; ok && now.Sub(last) < time.Hour {
		s.shareMu.Unlock()
		return
	}
	s.shareTouched[sh.ID] = now
	s.shareMu.Unlock()
	s.st.DB.Model(&store.ShareSubscription{}).Where("id = ?", sh.ID).Update("last_used_at", now)
}

// sweepShareExpiries pushes fresh specs for shares whose own expiry has
// just passed: served-ness is computed, so nothing needs persisting — the
// sweep only makes sure node configs drop the share promptly instead of
// waiting for the next unrelated change. Each expiry is handled once per
// instance (a restart simply re-marks the affected nodes once).
func (s *Server) sweepShareExpiries(now time.Time) {
	var rows []store.ShareSubscription
	if err := s.st.DB.Select("id", "user_id", "expire_at").
		Where("enabled = ? AND expire_at IS NOT NULL", true).Find(&rows).Error; err != nil {
		s.log.Warn("could not read share expiries", "err", err)
		return
	}
	s.shareMu.Lock()
	if s.shareSwept == nil {
		s.shareSwept = map[string]time.Time{}
	}
	var owners []string
	for i := range rows {
		sh := &rows[i]
		if !sh.Expired(now) {
			continue
		}
		if done, ok := s.shareSwept[sh.ID]; ok && done.Equal(*sh.ExpireAt) {
			continue
		}
		s.shareSwept[sh.ID] = *sh.ExpireAt
		owners = append(owners, sh.UserID)
	}
	s.shareMu.Unlock()
	if len(owners) > 0 {
		s.MarkDirty("share expired", s.nodesForUsers(owners...)...)
	}
}
