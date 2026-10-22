package panel

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

type templateFieldChange struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
	Kept   bool   `json:"kept"`
}

func configObject(value any) map[string]any {
	b, _ := json.Marshal(value)
	var object map[string]any
	_ = json.Unmarshal(b, &object)
	if object == nil {
		object = map[string]any{}
	}
	return object
}

func configChanges(prefix string, before, after map[string]any) []templateFieldChange {
	keys := map[string]bool{}
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var changes []templateFieldChange
	for _, key := range ordered {
		a, b := before[key], after[key]
		if reflect.DeepEqual(a, b) {
			continue
		}
		field := prefix + "." + key
		am, aMap := a.(map[string]any)
		bm, bMap := b.(map[string]any)
		if aMap && bMap {
			changes = append(changes, configChanges(field, am, bm)...)
		} else {
			changes = append(changes, templateFieldChange{Field: field, Before: a, After: b})
		}
	}
	return changes
}

func configOverridden(field string, overrides map[string]bool) bool {
	for path, enabled := range overrides {
		if enabled && (path == field || strings.HasPrefix(field, path+".") || strings.HasPrefix(path, field+".")) {
			return true
		}
	}
	return false
}

func setConfigValue(object map[string]any, path []string, value any) {
	if len(path) == 1 {
		if value == nil {
			delete(object, path[0])
		} else {
			object[path[0]] = value
		}
		return
	}
	child, ok := object[path[0]].(map[string]any)
	if !ok {
		child = map[string]any{}
		object[path[0]] = child
	}
	setConfigValue(child, path[1:], value)
}

func safeConfigChange(change templateFieldChange) templateFieldChange {
	if contains([]string{"protocol.visionUDP443", "protocol.udp", "transport.acceptProxyProtocol", "sniffing.enabled", "sniffing.routeOnly", "sniffing.metadataOnly"}, change.Field) {
		if change.Before == nil {
			change.Before = false
		}
		if change.After == nil {
			change.After = false
		}
	}
	key := strings.ToLower(change.Field)
	if strings.Contains(key, "privatekey") || strings.Contains(key, "serverkey") || strings.Contains(key, "password") || strings.Contains(key, "decryption") || strings.Contains(key, "mldsa65seed") || strings.HasSuffix(key, ".keypem") {
		mask := func(value any) any {
			if value == nil || value == "" {
				return "未设置"
			}
			return "已设置"
		}
		change.Before, change.After = mask(change.Before), mask(change.After)
	}
	return change
}

// JSON leaf paths keep old group-level overrides readable while new edits
// protect only the fields that were actually changed.
func mergeTemplateGroup(prefix string, before, want, destination any, overrides, selected map[string]bool) (fields, kept []string, details []templateFieldChange) {
	object := configObject(before)
	for _, change := range configChanges(prefix, object, configObject(want)) {
		if selected != nil && !selected[change.Field] {
			continue
		}
		change.Kept = configOverridden(change.Field, overrides)
		if change.Kept {
			kept = append(kept, change.Field)
		} else {
			fields = append(fields, change.Field)
			setConfigValue(object, strings.Split(strings.TrimPrefix(change.Field, prefix+"."), "."), change.After)
		}
		details = append(details, safeConfigChange(change))
	}
	b, _ := json.Marshal(object)
	reflect.ValueOf(destination).Elem().SetZero()
	_ = json.Unmarshal(b, destination)
	return
}
