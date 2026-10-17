//go:build !pro

package agent

import (
	"log/slog"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Community builds have no per-user speed limiter (a paid feature, built
// from the polaris-pro sources with -tags pro). newLimiter returns nil and
// every caller checks for nil, so cores are never wired to a limiter.

type limiter struct{}

type limitStat struct {
	User             string
	UpKbps, DownKbps int
	UpBps, DownBps   int64
}

func newLimiter(string, *slog.Logger) *limiter { return nil }

func (*limiter) setLimits([]model.UserLimit)                  {}
func (*limiter) stats() []limitStat                           { return nil }
func (*limiter) wire(listen, reentry string) *model.LimitWire { return nil }
func (*limiter) close()                                       {}
