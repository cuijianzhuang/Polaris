package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"gorm.io/gorm/schema"
)

// rawJSONSerializer stores a json.RawMessage in a text column. PostgreSQL
// returns text as a string, which database/sql cannot scan into a
// json.RawMessage by itself.
type rawJSONSerializer struct{}

func init() { schema.RegisterSerializer("rawjson", rawJSONSerializer{}) }

func (rawJSONSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, dbValue any) error {
	var raw json.RawMessage
	switch v := dbValue.(type) {
	case nil:
	case string:
		if v != "" {
			raw = json.RawMessage(v)
		}
	case []byte:
		if len(v) > 0 {
			raw = append(json.RawMessage(nil), v...)
		}
	default:
		return fmt.Errorf("%s: cannot read %T as JSON", field.DBName, dbValue)
	}
	field.ReflectValueOf(ctx, dst).Set(reflect.ValueOf(raw))
	return nil
}

func (rawJSONSerializer) Value(_ context.Context, _ *schema.Field, _ reflect.Value, fieldValue any) (any, error) {
	raw, _ := fieldValue.(json.RawMessage)
	if raw == nil {
		return nil, nil
	}
	return string(raw), nil
}
