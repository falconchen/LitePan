//go:build fuse

package fuse

import (
	"litepan/internal/domain"
)

type backend struct {
	deps  Deps
	mount *domain.FuseMount
}

func (b *backend) warn(msg string, args ...any) {
	if b == nil || b.deps.Log == nil {
		return
	}
	b.deps.Log.Warn(msg, args...)
}

func (b *backend) accountID() int64 {
	if b == nil || b.mount == nil {
		return 0
	}
	return b.mount.AccountID
}
