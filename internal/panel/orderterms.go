package panel

import (
	"encoding/json"
	"errors"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

// Account limits are bought at checkout. Node access, speed and fallback
// routing remain the live plan's policy, as for existing subscribers.
type orderTerms struct {
	Version int                `json:"version"`
	Plan    *store.Group       `json:"plan,omitempty"`
	Pack    *store.TrafficPack `json:"pack,omitempty"`
}

func freezePlan(p *store.Group) string {
	b, _ := json.Marshal(orderTerms{Version: 1, Plan: &store.Group{ID: p.ID, Name: p.Name, Plan: true,
		Days: p.Days, TrafficLimit: p.TrafficLimit, ResetPolicy: p.ResetPolicy, ResetDay: p.ResetDay, DeviceLimit: p.DeviceLimit, IPLimit: p.IPLimit}})
	return string(b)
}
func freezePack(p *store.TrafficPack) string {
	b, _ := json.Marshal(orderTerms{Version: 1, Pack: &store.TrafficPack{ID: p.ID, Name: p.Name, Traffic: p.Traffic, PlanIDs: p.PlanIDs}})
	return string(b)
}
func readOrderTerms(o *store.Order) (orderTerms, error) {
	var v orderTerms
	if err := json.Unmarshal([]byte(o.Entitlements), &v); err != nil || v.Version != 1 {
		return v, errors.New("invalid order entitlement snapshot")
	}
	return v, nil
}
func orderPlan(tx *gorm.DB, o *store.Order, p *store.Group) error {
	// A deleted plan no longer grants node access; retain the existing
	// failed-payment path rather than claim that an unusable plan was delivered.
	if err := tx.First(p, "id = ? AND plan = ?", o.PlanID, true).Error; err != nil {
		return err
	}
	if o.Entitlements == "" {
		return nil
	} // historical limits cannot be reconstructed
	v, err := readOrderTerms(o)
	if err != nil || v.Plan == nil || v.Pack != nil || v.Plan.ID != o.PlanID || v.Plan.TrafficLimit < 0 || v.Plan.Days < 0 {
		return errors.New("invalid order plan snapshot")
	}
	*p = *v.Plan
	return nil
}
func orderPack(tx *gorm.DB, o *store.Order, p *store.TrafficPack) error {
	if o.Entitlements == "" {
		return tx.First(p, "id = ?", o.PackID).Error
	}
	v, err := readOrderTerms(o)
	if err != nil || v.Pack == nil || v.Plan != nil || v.Pack.ID != o.PackID || v.Pack.Traffic <= 0 {
		return errors.New("invalid order pack snapshot")
	}
	*p = *v.Pack
	return nil
}
