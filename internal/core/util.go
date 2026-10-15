package core

import (
	"encoding/json"
	"strconv"
	"strings"
)

// VersionAtLeast compares dotted versions ("v1.12.0", "25.3.6"). An empty or
// "latest" version is treated as newest.
func VersionAtLeast(v, min string) bool {
	if v == "" || v == "latest" {
		return true
	}
	a, b := parseVer(v), parseVer(min)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

func parseVer(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out
}

// MergeExtra deep-merges a raw JSON object into dst. Objects merge
// recursively; every other value replaces the existing one.
func MergeExtra(dst map[string]any, extra json.RawMessage) error {
	if len(extra) == 0 {
		return nil
	}
	var src map[string]any
	if err := json.Unmarshal(extra, &src); err != nil {
		return err
	}
	deepMerge(dst, src)
	return nil
}

func deepMerge(dst, src map[string]any) {
	for k, sv := range src {
		if sm, ok := sv.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = sv
	}
}

// PEMLines splits a PEM block into lines (Xray accepts inline certs as arrays).
func PEMLines(pem string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(pem), "\n") {
		out = append(out, strings.TrimRight(l, "\r"))
	}
	return out
}
