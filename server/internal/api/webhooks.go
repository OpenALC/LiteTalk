package api

// webhooks.go 入站与出站 Webhook REST（#52）。出站是服务器级事件订阅
// （ManageServer），入站是频道级投递入口（ManageChannels）；对外投递入口
// POST /api/v1/webhooks/{id}/{secret} 是全服务唯一免 Bearer 端点——凭路径
// secret 常数时间校验，失败一律 404（不区分 id 不存在与 secret 错，避免枚举）。
// 签名/重试/熔断等投递逻辑在 internal/webhook，这里只做 CRUD 与入站落库。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/webhook"
)

// 频控（键前缀 wh: 管理写端点 / inb: 入站投递，批次 6 §0.4）。
// 管理动作按分钟计；入站投递按 webhook id 维度（防外部系统刷屏与 secret 爆破）。
const (
	whCreateLimit     = 30
	whPatchLimit      = 30
	whDeleteLimit     = 30
	whInboundLimit    = 30
	whInboundWindow   = time.Minute
	inboundDescMax    = 255
	inboundContentMax = 2000
	// inboundBotEmail 占位机器人身份的固定邮箱（bots.invalid 保留域，无法收信、
	// 无法密码登录——password_hash 落空串，CheckPassword 对空哈希恒 false）。
	// T4（bot 账号模型）合入后需把该占位接线为真正的 bot 用户（is_bot=1）。
	inboundBotEmail = "webhook@bots.invalid"
	// disabledReasonManual 手动停用写入 disabled_reason 的值。
	disabledReasonManual = "manual"
)

// ---- Merge Patch 体（RFC 7396；三态语义与 mergepatch.go 一致）----
// 本文件的 PATCH 体放在这里而不是 mergepatch.go：webhook 的事件数组与布尔键
// 是本域特有形状，避免为一次性用途扩公共三态类型集。

// OptBool 合并补丁布尔字段：缺省不改；null 按未设置处理（布尔无「清空」语义）。
type OptBool struct {
	Set  bool
	Null bool
	Val  bool
}

func (o *OptBool) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Val)
}

// OptStrings 合并补丁字符串数组字段：缺省不改；null = 清空（由 handler 决定是否合法）。
type OptStrings struct {
	Set  bool
	Null bool
	Val  []string
}

func (o *OptStrings) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		o.Val = nil
		return nil
	}
	return json.Unmarshal(b, &o.Val)
}

// PatchWebhookBody PATCH /servers/{id}/webhooks/{wid} 合并补丁体（#52）：
// url / events / enabled / reset_secret 四键，缺省不改。
type PatchWebhookBody struct {
	Url         OptString
	Events      OptStrings
	Enabled     OptBool
	ResetSecret OptBool
}

// UnmarshalJSON 三态解码（键存在性由 decodeMergePatch 探测）。
func (b *PatchWebhookBody) UnmarshalJSON(data []byte) error {
	return decodeMergePatch(data, map[string]mergeField{
		"url": &b.Url, "events": &b.Events,
		"enabled": &b.Enabled, "reset_secret": &b.ResetSecret,
	})
}

// ---- 出站订阅 ----

// ListServerWebhooks GET /api/v1/servers/{id}/webhooks（ManageServer）。
func (s *Server) ListServerWebhooks(ctx context.Context, request ListServerWebhooksRequestObject) (ListServerWebhooksResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageServer); e != nil {
		return nil, e
	}
	rows, err := s.store.OutboundWebhooksOfServer(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]OutboundWebhook, 0, len(rows))
	for i := range rows {
		out = append(out, outboundWebhookWire(&rows[i], ""))
	}
	return ListServerWebhooks200JSONResponse(out), nil
}

// CreateServerWebhook POST /api/v1/servers/{id}/webhooks：URL 过 SSRF 校验、
// 事件过白名单校验，secret 只在响应出现一次。
func (s *Server) CreateServerWebhook(ctx context.Context, request CreateServerWebhookRequestObject) (CreateServerWebhookResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageServer); e != nil {
		return nil, e
	}
	if e := s.allowRate(ctx, "wh:create:"+strconv.FormatInt(c.UserID, 10),
		whCreateLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	events := make([]string, 0, len(request.Body.Events))
	for _, ev := range request.Body.Events {
		events = append(events, string(ev))
	}
	if !webhook.ValidEvents(events) {
		return nil, errf(http.StatusBadRequest, "bad_events",
			"events 必须非空、不重复且全部在白名单内")
	}
	secret, e := s.newWebhookSecret()
	if e != nil {
		return nil, e
	}
	w := &store.OutboundWebhook{
		ServerID: request.Id,
		URL:      request.Body.Url,
		Secret:   secret,
		Events:   joinEvents(events),
	}
	if e := s.checkWebhookURL(ctx, w.URL); e != nil {
		return nil, e
	}
	if err := s.store.CreateOutboundWebhook(ctx, w); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 首个订阅诞生：惰性启动投递 worker（零订阅部署保持零后台 goroutine）。
	s.webhooks.EnsureStarted()
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "webhook.create",
		TargetType: "webhook", TargetID: ptrID(w.ID),
		Detail: auditDetail(map[string]any{"events": events}),
	})
	s.log.Printf("[audit] webhook created server=%d id=%d events=%q ip=%s",
		request.Id, w.ID, w.Events, reqIP(ctx))
	return CreateServerWebhook201JSONResponse(outboundWebhookWire(w, secret)), nil
}

// PatchServerWebhook PATCH /api/v1/servers/{id}/webhooks/{wid}（Merge Patch）：
// url / events 增量改；reset_secret=true 换新 secret（旧 secret 立即失效，明文只在
// 本次响应返回）；enabled=true 手动重新启用并清零连续失败计数（熔断恢复唯一途径）。
func (s *Server) PatchServerWebhook(ctx context.Context, request PatchServerWebhookRequestObject) (PatchServerWebhookResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(int64(request.Wid)); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageServer); e != nil {
		return nil, e
	}
	if e := s.allowRate(ctx, "wh:patch:"+strconv.FormatInt(c.UserID, 10),
		whPatchLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	body := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if body == nil {
		return nil, bodyMissing()
	}
	w, err := s.store.OutboundWebhookByID(ctx, int64(request.Wid))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	if w.ServerID != request.Id {
		// 归属不符与不存在同响应：不泄露跨服资源存在性。
		return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	secretOut := ""
	if body.Url.Set {
		if e := s.checkWebhookURL(ctx, body.Url.Val); e != nil {
			return nil, e
		}
		w.URL = body.Url.Val
	}
	if body.Events.Set {
		if body.Events.Null || !webhook.ValidEvents(body.Events.Val) {
			return nil, errf(http.StatusBadRequest, "bad_events",
				"events 必须非空、不重复且全部在白名单内")
		}
		w.Events = joinEvents(body.Events.Val)
	}
	if body.ResetSecret.Set && body.ResetSecret.Val {
		// 旧 secret 立即失效：新 secret 原子落库；明文只在本次响应出现一次。
		secret, e := s.newWebhookSecret()
		if e != nil {
			return nil, e
		}
		w.Secret = secret
		secretOut = secret
	}
	if body.Enabled.Set && !body.Enabled.Null {
		if body.Enabled.Val {
			w.Disabled = false
			w.DisabledReason = ""
			w.FailCount = 0 // 手动重新启用清零连败计数
		} else {
			w.Disabled = true
			w.DisabledReason = disabledReasonManual
		}
	}
	if err := s.store.UpdateOutboundWebhook(ctx, w); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if secretOut != "" {
		s.audit(ctx, store.AuditLog{
			ServerID: request.Id, ActorID: c.UserID, Action: "webhook.reset_secret",
			TargetType: "webhook", TargetID: ptrID(w.ID), Detail: "{}",
		})
	}
	return PatchServerWebhook200JSONResponse(outboundWebhookWire(w, secretOut)), nil
}

// DeleteServerWebhook DELETE /api/v1/servers/{id}/webhooks/{wid}：立即不再投递。
func (s *Server) DeleteServerWebhook(ctx context.Context, request DeleteServerWebhookRequestObject) (DeleteServerWebhookResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(int64(request.Wid)); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageServer); e != nil {
		return nil, e
	}
	if e := s.allowRate(ctx, "wh:del:"+strconv.FormatInt(c.UserID, 10),
		whDeleteLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	w, err := s.store.OutboundWebhookByID(ctx, int64(request.Wid))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	if w.ServerID != request.Id {
		return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	if err := s.store.DeleteOutboundWebhook(ctx, int64(request.Wid)); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "webhook.delete",
		TargetType: "webhook", TargetID: ptrID(w.ID), Detail: "{}",
	})
	return DeleteServerWebhook200JSONResponse{Status: StatusOkStatusOk}, nil
}

// ---- 入站投递入口 ----

// ListInboundWebhooks GET /api/v1/channels/{id}/inbound-webhooks（ManageChannels）。
func (s *Server) ListInboundWebhooks(ctx context.Context, request ListInboundWebhooksRequestObject) (ListInboundWebhooksResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, e := s.inboundChannel(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	rows, err := s.store.InboundWebhooksOfChannel(ctx, ch.ID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]InboundWebhook, 0, len(rows))
	for i := range rows {
		out = append(out, inboundWebhookWire(&rows[i], ""))
	}
	return ListInboundWebhooks200JSONResponse(out), nil
}

// CreateInboundWebhook POST /api/v1/channels/{id}/inbound-webhooks：
// 响应带一次性投递 URL（含 secret 明文）。
func (s *Server) CreateInboundWebhook(ctx context.Context, request CreateInboundWebhookRequestObject) (CreateInboundWebhookResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, e := s.inboundChannel(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	if e := s.allowRate(ctx, "wh:create-inb:"+strconv.FormatInt(c.UserID, 10),
		whCreateLimit, time.Minute, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	desc := ""
	if request.Body != nil && request.Body.Description != nil {
		desc = *request.Body.Description
		if len([]rune(desc)) > inboundDescMax {
			return nil, errf(http.StatusBadRequest, "bad_description", "description 需为 0–255 字符")
		}
	}
	secret, e := s.newWebhookSecret()
	if e != nil {
		return nil, e
	}
	w := &store.InboundWebhook{ChannelID: ch.ID, Secret: secret, Description: desc}
	if err := s.store.CreateInboundWebhook(ctx, w); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.audit(ctx, store.AuditLog{
		ServerID: ch.ServerID, ActorID: c.UserID, Action: "inbound_webhook.create",
		TargetType: "webhook", TargetID: ptrID(w.ID), ChannelID: ptrID(ch.ID), Detail: "{}",
	})
	return CreateInboundWebhook201JSONResponse(inboundWebhookWire(w, inboundDeliveryPath(w.ID, secret))), nil
}

// DeliverInboundWebhook POST /api/v1/webhooks/{id}/{secret}（免 Bearer）：
// secret 常数时间比对，失败一律 404；消息以占位机器人身份落库。
func (s *Server) DeliverInboundWebhook(ctx context.Context, request DeliverInboundWebhookRequestObject) (DeliverInboundWebhookResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	id := int64(request.Id)
	// 频控先于校验（键 inb:<id>）：同 id 的高频探测被挡在库查询之前；
	// 首次探测无论 id 存在与否都过频控，429/404 口径不泄露存在性。
	if e := s.allowRate(ctx, "inb:"+strconv.FormatInt(id, 10),
		whInboundLimit, whInboundWindow, "操作过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	w, err := s.store.InboundWebhookByID(ctx, id)
	if err != nil {
		return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	// 常数时间比对：不区分 id 不存在与 secret 错，二者同样 404。
	if subtle.ConstantTimeCompare([]byte(w.Secret), []byte(request.Secret)) != 1 {
		return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	content := request.Body.Content
	if n := len([]rune(content)); n < 1 || n > inboundContentMax {
		return nil, errf(http.StatusBadRequest, "bad_content", "消息长度需为 1–2000 字符")
	}
	ch, err := s.store.ChannelByID(ctx, w.ChannelID)
	if err != nil {
		// 入口指向的频道已删除：URL 随之失效，与鉴权失败同口径 404。
		return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	if ch.Type != store.ChannelText {
		return nil, errf(http.StatusBadRequest, "bad_channel", "只能向文字频道投递消息")
	}
	bot, err := s.inboundBotUser(ctx)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	msg := &store.Message{ChannelID: ch.ID, UserID: bot.ID, Content: content}
	if err := s.store.CreateMessage(ctx, msg); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return DeliverInboundWebhook200JSONResponse{Status: StatusOkStatusOk}, nil
}

// ---- 辅助 ----

// inboundChannel 入站管理的频道门：频道存在（404）、服务器作用域且为文字频道
// （400 bad_channel）、调用者有 ManageChannels（403）。
func (s *Server) inboundChannel(ctx context.Context, channelID, userID int64) (*store.Channel, *apiErr) {
	ch, err := s.store.ChannelByID(ctx, channelID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := serverChannelOnly(ch); e != nil {
		return nil, errf(http.StatusBadRequest, "bad_channel", "入站投递只支持服务器文字频道")
	}
	if ch.Type != store.ChannelText {
		return nil, errf(http.StatusBadRequest, "bad_channel", "入站投递只支持服务器文字频道")
	}
	if e := s.requireChannelPerm(ctx, ch, userID, store.ManageChannels); e != nil {
		return nil, e
	}
	return ch, nil
}

// checkWebhookURL 出站目标的 SSRF 校验（创建/更新时）。checkURL 由 New 注入
// 默认 linkpreview.CheckDialable，测试可替换——生产语义：拒绝内网段/回环/
// link-local/云元数据地址与 localhost，域名即时解析逐 IP 判定，违规 400 bad_url
// 且不回传解析细节。投递时另有拨号前二次校验（internal/webhook.safeDial）。
func (s *Server) checkWebhookURL(_ context.Context, rawURL string) *apiErr {
	if rawURL == "" || len(rawURL) > 2048 {
		return errf(http.StatusBadRequest, "bad_url", "url 不合法")
	}
	if err := s.webhookURLCheck(rawURL); err != nil {
		return errf(http.StatusBadRequest, "bad_url", "url 不被允许（内网/保留地址或格式非法）")
	}
	return nil
}

// newWebhookSecret 生成 64 位 hex 投递密钥（出站签名 / 入站路径校验共用形态）。
func (s *Server) newWebhookSecret() (string, *apiErr) {
	secret, err := webhook.NewSecret()
	if err != nil {
		return "", s.writeStoreErr(context.Background(), err)
	}
	return secret, nil
}

// joinEvents 白名单事件去重后落库（空格分隔；重复在 ValidEvents 已拒）。
func joinEvents(events []string) string {
	seen := make(map[string]bool, len(events))
	out := make([]string, 0, len(events))
	for _, e := range events {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return strings.Join(out, " ")
}

// inboundBotUser 占位机器人身份（懒创建，见 inboundBotEmail 注释）：按固定邮箱
// 回读，不存在则建号（随机 4 位 tag 防撞唯一索引，撞了重摇并回读）。并发首投递
// 的唯一键竞争由重试覆盖；头像与邮箱验证字段保持缺省。
func (s *Server) inboundBotUser(ctx context.Context) (*store.User, error) {
	if u, err := s.store.UserByEmail(ctx, inboundBotEmail); err == nil {
		return u, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	for i := 0; i < 5; i++ {
		tag, err := randTag()
		if err != nil {
			return nil, err
		}
		u := &store.User{
			Email:    inboundBotEmail,
			Nickname: "Webhook",
			Tag:      tag,
			// password_hash 空串：CheckPassword 对空哈希恒 false，无法密码登录。
			PasswordHash: "",
		}
		err = s.store.CreateUser(ctx, u)
		if err == nil {
			return u, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
		// tag 撞唯一索引：重摇；邮箱被并发先建则下一次循环直接回读命中。
	}
	// 最后兜底回读（并发创建方已完成）。
	return s.store.UserByEmail(ctx, inboundBotEmail)
}

// randTag 4 位定宽数字串（users.tag 口径）。
func randTag() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", fmt.Errorf("生成 tag 失败: %w", err)
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}

// outboundWebhookWire store 行 → wire。secretOut 非空时（创建 / reset_secret）
// 才回传一次性明文；其余路径永不回传。
func outboundWebhookWire(w *store.OutboundWebhook, secretOut string) OutboundWebhook {
	out := OutboundWebhook{
		Id:        idOf(w.ID),
		Url:       w.URL,
		Events:    splitEvents(w.Events),
		Disabled:  w.Disabled,
		CreatedAt: timeOf(w.CreatedAt),
	}
	for i, e := range out.Events {
		out.Events[i] = OutboundWebhookEvents(e)
	}
	if w.DisabledReason != "" {
		r := w.DisabledReason
		out.DisabledReason = &r
	}
	if secretOut != "" {
		s := secretOut
		out.Secret = &s
	}
	return out
}

// splitEvents 空格分隔 → 数组（wire 用）。
func splitEvents(events string) []OutboundWebhookEvents {
	fields := strings.Fields(events)
	out := make([]OutboundWebhookEvents, 0, len(fields))
	for _, f := range fields {
		out = append(out, OutboundWebhookEvents(f))
	}
	return out
}

// inboundWebhookWire store 行 → wire。urlOut 非空时（创建响应）带一次性投递路径。
func inboundWebhookWire(w *store.InboundWebhook, urlOut string) InboundWebhook {
	out := InboundWebhook{
		Id:          idOf(w.ID),
		Description: w.Description,
		CreatedAt:   timeOf(w.CreatedAt),
	}
	if urlOut != "" {
		u := urlOut
		out.Url = &u
	}
	return out
}

// inboundDeliveryPath 入站投递路径（相对路径，含一次性 secret）。
func inboundDeliveryPath(id int64, secret string) string {
	return "/api/v1/webhooks/" + strconv.FormatInt(id, 10) + "/" + secret
}
