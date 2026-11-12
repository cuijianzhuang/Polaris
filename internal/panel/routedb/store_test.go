package routedb

import (
	"errors"
	"testing"
)

type fakeSettingStore struct {
	values map[string]string
	err    error
}

func (f *fakeSettingStore) Setting(key string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.values[key]
	return v, ok, nil
}

func TestReadPolicy(t *testing.T) {
	p, err := ReadPolicy(&fakeSettingStore{values: map[string]string{}}, "route-asn", errInvalid)
	if err != nil || p != (Policy{}) {
		t.Fatalf("missing row: %+v %v", p, err)
	}
	p, err = ReadPolicy(&fakeSettingStore{values: map[string]string{
		"route-asn": `{"enabled":true,"sha256":"` + hash64 + `","revision":3}`,
	}}, "route-asn", errInvalid)
	if err != nil || !p.Enabled || p.SHA256 != hash64 || p.Revision != 3 {
		t.Fatalf("valid row: %+v %v", p, err)
	}
	_, err = ReadPolicy(&fakeSettingStore{values: map[string]string{"route-asn": "not json"}}, "route-asn", errInvalid)
	if !errors.Is(err, errInvalid) {
		t.Fatalf("malformed row: %v", err)
	}
	storeErr := errors.New("db down")
	_, err = ReadPolicy(&fakeSettingStore{err: storeErr}, "route-asn", errInvalid)
	if err != storeErr {
		t.Fatalf("store error: %v", err)
	}
}
