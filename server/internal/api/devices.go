package api

// devices.go 移动推送设备注册 REST（#53）。派发与触发判定在 internal/push，
// 这里只做注册表的 CRUD：鉴权 + 归属校验 + 频控（键前缀 push:，批次 6 §0.4）。
// push_token 是生产凭据的等价物：不进任何响应，日志打码只留前 8 位。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// 频控（键前缀 push:）：注册/注销都是客户端生命周期动作，按分钟计。
const (
	deviceRegisterLimit = 30
	deviceDeleteLimit   = 30
	// pushTokenMaxLen FCM token 可达 4KB；上限放 4096 已覆盖全部通道形态。
	pushTokenMaxLen = 4096
)

// ListDevices GET /api/v1/devices：当前用户的全部设备（不含 push_token）。
func (s *Server) ListDevices(ctx context.Context, _ ListDevicesRequestObject) (ListDevicesResponseObject, error) {
	c := currentUser(ctx)
	rows, err := s.store.PushDevicesOfUser(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]PushDevice, 0, len(rows))
	for i := range rows {
		out = append(out, pushDeviceWire(&rows[i]))
	}
	return ListDevices200JSONResponse{Devices: out}, nil
}

// RegisterDevice POST /api/v1/devices：幂等注册。同一 push_token 重复注册 =
// 更新 platform/last_seen（200 既有设备），新设备 201。
func (s *Server) RegisterDevice(ctx context.Context, request RegisterDeviceRequestObject) (RegisterDeviceResponseObject, error) {
	c := currentUser(ctx)
	if e := s.allowRate(ctx, "push:reg:"+strconv.FormatInt(c.UserID, 10),
		deviceRegisterLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	platform := string(request.Body.Platform)
	if platform != "ios" && platform != "android" {
		return nil, errf(http.StatusBadRequest, "bad_device", "platform 需为 ios 或 android")
	}
	token := request.Body.PushToken
	if token == "" || len(token) > pushTokenMaxLen {
		return nil, errf(http.StatusBadRequest, "bad_token", "push_token 需为 1–4096 字符")
	}
	d := &store.PushDevice{UserID: c.UserID, Platform: platform, Token: token}
	created, err := s.store.UpsertPushDevice(ctx, d)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 重复注册（更新分支）按 200 回既有设备；新设备 201。
	if created {
		return RegisterDevice201JSONResponse(pushDeviceWire(d)), nil
	}
	return RegisterDevice200JSONResponse(pushDeviceWire(d)), nil
}

// DeleteDevice DELETE /api/v1/devices/{id}：只能删自己的设备。他人设备与不存在
// 统一 404——不泄露「设备是否存在」这一跨用户信息。
func (s *Server) DeleteDevice(ctx context.Context, request DeleteDeviceRequestObject) (DeleteDeviceResponseObject, error) {
	c := currentUser(ctx)
	id := int64(request.Id)
	if e := checkID(id); e != nil {
		return nil, e
	}
	if e := s.allowRate(ctx, "push:del:"+strconv.FormatInt(c.UserID, 10),
		deviceDeleteLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	d, err := s.store.PushDeviceByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	if d.UserID != c.UserID {
		// 归属不符与不存在同响应：设备行不泄露跨用户存在性。
		return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	if err := s.store.DeletePushDevice(ctx, id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return DeleteDevice200JSONResponse{Status: StatusOkStatusOk}, nil
}

// pushDeviceWire store 行 → wire（无 token 字段，凭据永不回显）。
func pushDeviceWire(d *store.PushDevice) PushDevice {
	return PushDevice{
		Id:         idOf(d.ID),
		Platform:   PushDevicePlatform(d.Platform),
		CreatedAt:  timeOf(d.CreatedAt),
		LastSeenAt: timeOf(d.LastSeen),
	}
}

// pushEnabled 移动推送是否已配置可用通道（Noop = 关闭）。
func (s *Server) pushEnabled() bool { return s.push != nil }
