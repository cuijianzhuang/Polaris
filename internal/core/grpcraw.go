package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// The Xray and sing-box management APIs are plain gRPC services. Rather than
// importing either core's Go module (huge, and GPL for sing-box) we hand-encode
// the handful of messages we need with protowire and send them as raw bytes.

type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: unexpected %T", v)
	}
	return *b, nil
}
func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: unexpected %T", v)
	}
	*b = append((*b)[:0], data...)
	return nil
}

var _ encoding.Codec = rawCodec{}

// APIClient is a raw gRPC connection to a core's loopback management API.
type APIClient struct {
	conn *grpc.ClientConn
}

// DialAPI connects lazily to addr (loopback, plaintext).
func DialAPI(addr string) (*APIClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))
	if err != nil {
		return nil, err
	}
	return &APIClient{conn: conn}, nil
}

func (c *APIClient) Close() error { return c.conn.Close() }

// Invoke calls a unary method with a pre-encoded request.
func (c *APIClient) Invoke(ctx context.Context, method string, req []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var resp []byte
	if err := c.conn.Invoke(ctx, method, &req, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// IsUnimplemented reports a gRPC Unimplemented error.
func IsUnimplemented(err error) bool { return status.Code(err) == codes.Unimplemented }

// Stat is one counter from StatsService.
type Stat struct {
	Name  string
	Value int64
}

// QueryStats calls <service>/QueryStats{pattern, reset}.
func (c *APIClient) QueryStats(ctx context.Context, service, pattern string, reset bool) ([]Stat, error) {
	var req []byte
	req = PBString(req, 1, pattern)
	req = PBBool(req, 2, reset)
	resp, err := c.Invoke(ctx, "/"+service+"/QueryStats", req)
	if err != nil {
		return nil, err
	}
	var out []Stat
	err = PBRange(resp, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) {
		if num != 1 || typ != protowire.BytesType {
			return
		}
		var st Stat
		_ = PBRange(v, func(n protowire.Number, t protowire.Type, b []byte, u uint64) {
			switch {
			case n == 1 && t == protowire.BytesType:
				st.Name = string(b)
			case n == 2 && t == protowire.VarintType:
				st.Value = int64(u)
			}
		})
		out = append(out, st)
	})
	return out, err
}

// UserTraffic folds "user>>>{id}>>>traffic>>>uplink|downlink" counters into
// per-user up/down totals.
func UserTraffic(stats []Stat) map[string][2]int64 {
	m := map[string][2]int64{}
	for _, s := range stats {
		parts := strings.Split(s.Name, ">>>")
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
			continue
		}
		v := m[parts[1]]
		switch parts[3] {
		case "uplink":
			v[0] += s.Value
		case "downlink":
			v[1] += s.Value
		}
		m[parts[1]] = v
	}
	return m
}

// --- protowire helpers ---

func PBString(b []byte, num protowire.Number, s string) []byte {
	if s == "" {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func PBBytes(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func PBBool(b []byte, num protowire.Number, v bool) []byte {
	if !v {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, 1)
}

func PBVarint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

// PBTyped encodes a TypedMessage{type=1, value=2}.
func PBTyped(typeName string, value []byte) []byte {
	var b []byte
	b = PBString(b, 1, typeName)
	return PBBytes(b, 2, value)
}

// PBRange iterates the top-level fields of an encoded message.
func PBRange(b []byte, fn func(num protowire.Number, typ protowire.Type, bytesVal []byte, varint uint64)) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			fn(num, typ, nil, v)
			b = b[n:]
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			fn(num, typ, v, 0)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}
