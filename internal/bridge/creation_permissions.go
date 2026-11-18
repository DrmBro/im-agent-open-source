package bridge

import (
	"database/sql"
	"errors"

	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) checkCreationPermissionFailure(address im.Address) error {
	id, err := b.store.GetMeta("creation-permission-unconfirmed:" + store.Hash(address.Key()))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id != "" {
		return diagnostics.UserError("此前已创建会话 " + id + "，但 Full Access 权限未确认；请核对原会话，本入口未重复创建")
	}
	return nil
}
func (b *Bridge) recordCreationPermissionFailure(address im.Address, id string, cause error) error {
	if id == "" {
		return cause
	}
	if err := b.store.SetMeta("creation-permission-unconfirmed:"+store.Hash(address.Key()), id); err != nil {
		return diagnostics.UserError("已创建会话 " + id + "，权限与本地记录未确认；请核对原会话，勿重复提交")
	}
	return cause
}
