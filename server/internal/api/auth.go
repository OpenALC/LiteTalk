package api

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// ---- 请求/响应结构 ----

type registerReq struct {
	Nickname  string `json:"nickname"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	Code      string `json:"code"`
	Device    string `json:"device"`    // 设备类型，空=desktop
	Bootstrap string `json:"bootstrap"` // 可选：管理员邀请码（#27，空=不使用）
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Device   string `json:"device"`
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutReq struct {
	RefreshToken string `json:"refresh_token"`
}

type emailCodeReq struct {
	Email   string `json:"email"`
	Purpose string `json:"purpose"` // register | reset
}

// deviceType 校验并回落设备类型（空→desktop）。白名单与 auth.DeviceTypes 共用（P2-2）：
// 新增设备类型只需改 auth 包一处；非法值（如 tablet）一律 400。
func deviceType(s string) (string, bool) {
	if s == "" {
		return auth.DeviceDesktop, true
	}
	if !auth.IsKnownDevice(s) {
		return "", false
	}
	return s, true
}

type passwordResetReq struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

// tokenPair 认证响应。
type tokenPair struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    int64       `json:"expires_in"` // 秒
	User         *store.User `json:"user"`
}

// ---- 校验 ----

// validEmail 简单邮箱格式校验。
func validEmail(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	_, err := mail.ParseAddress(s)
	return err == nil
}

// validNickname 昵称 2–16 个字符。
func validNickname(s string) bool {
	n := len([]rune(s))
	return n >= 2 && n <= 16
}

// validPassword 密码 8–72 字节（bcrypt 上限 72B，超出在入口就拒）。
func validPassword(s string) bool { return len(s) >= 8 && len(s) <= 72 }

// fmtTag 定宽 4 位。
func fmtTag(n int) string {
	s := strconv.Itoa(n % 10000)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// ---- handlers ----

// handleRegister POST /api/v1/auth/register（限流 10 次/h/IP：防批量注册与 bcrypt CPU 放大）
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ok, err := s.limits.Allow(r.Context(), "register:"+s.clientIP(r), 10, time.Hour)
	if err != nil || !ok {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "操作过于频繁，请稍后再试")
		return
	}
	var req registerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	dev, okDev := deviceType(req.Device)
	if !okDev {
		writeErr(w, http.StatusBadRequest, "bad_device", "设备类型不合法（仅 desktop/mobile）")
		return
	}
	req.Email = normalizeEmail(req.Email)
	if !validNickname(req.Nickname) {
		writeErr(w, http.StatusBadRequest, "bad_nickname", "昵称需为 2–16 个字符")
		return
	}
	if !validEmail(req.Email) {
		writeErr(w, http.StatusBadRequest, "bad_email", "邮箱格式不正确")
		return
	}
	if !validPassword(req.Password) {
		writeErr(w, http.StatusBadRequest, "bad_password", "密码需 8–72 个字符")
		return
	}
	// 管理员邀请码（#27）：带码则先校验合法且未消费，再走注册；
	// 不合法/已用 → 400 bad_bootstrap（不建用户，避免半成品账号）。
	if req.Bootstrap != "" {
		if err := s.checkBootstrap(r.Context(), req.Bootstrap); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_bootstrap", "邀请码无效或已被使用")
			return
		}
	}
	// 验证邮箱验证码（simple 模式免验证码）。
	if !s.simpleMode() {
		if err := s.codes.Verify(r.Context(), auth.PurposeRegister, req.Email, req.Code); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_code", "验证码无效或已过期")
			return
		}
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}

	// 分配 tag：4 位随机，冲突重试（UNIQUE(nickname,tag)）。
	var user *store.User
	for i := 0; i < 5; i++ {
		user = &store.User{
			Email:        req.Email,
			Nickname:     req.Nickname,
			Tag:          fmtTag(int(time.Now().UnixNano()+int64(i*7919)) % 10000),
			PasswordHash: hash,
			// full 模式邮箱已过验证码验证；simple 模式未验证（从简取舍）。
			EmailVerified: !s.simpleMode(),
		}
		err = s.store.CreateUser(r.Context(), user)
		if errors.Is(err, store.ErrConflict) {
			// 区分邮箱冲突与 tag 冲突。
			if _, e := s.store.UserByEmail(r.Context(), req.Email); e == nil {
				writeErr(w, http.StatusConflict, "email_taken", "邮箱已注册")
				return
			}
			continue // tag 冲突重试
		}
		break
	}
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	// 邀请码认领（#27）：以该用户为 owner 建默认服务器并作废该码。
	if req.Bootstrap != "" {
		s.claimBootstrap(r.Context(), req.Bootstrap, user)
	}
	s.issueTokens(w, r, user, dev, http.StatusCreated)
}

// handleGuest POST /api/v1/auth/guest（simple 模式专属，免注册一键进入）。
// 生成临时身份：游客-XXXX + 占位邮箱 + 空密码哈希（无法密码登录），
// 签发与普通注册同构的令牌对；客户端靠 refresh_token 维持身份。
func (s *Server) handleGuest(w http.ResponseWriter, r *http.Request) {
	if !s.simpleMode() {
		writeErr(w, http.StatusForbidden, "guest_disabled", "游客登录仅在 simple 模式开放")
		return
	}
	ok, err := s.limits.Allow(r.Context(), "guest:"+s.clientIP(r), 5, time.Minute)
	if err != nil || !ok {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "操作过于频繁，请稍后再试")
		return
	}
	var req struct {
		Device string `json:"device"`
	}
	// 请求体可为空（兼容老客户端）；非空时校验。
	if r.ContentLength > 0 {
		_ = decodeJSON(w, r, &req)
	}
	dev, okDev := deviceType(req.Device)
	if !okDev {
		writeErr(w, http.StatusBadRequest, "bad_device", "设备类型不合法（仅 desktop/mobile）")
		return
	}

	// 昵称/邮箱/标签全部随机，冲突重试（同 handleRegister 模式）
	var user *store.User
	for i := 0; i < 5; i++ {
		user = &store.User{
			Email:         fmt.Sprintf("guest-%s@guest.local", randHex(8)),
			Nickname:      "游客-" + randDigits(4),
			Tag:           randDigits(4),
			PasswordHash:  "", // 空哈希：bcrypt 校验必失败，无法密码登录
			EmailVerified: false,
			IsGuest:       true,
		}
		err = s.store.CreateUser(r.Context(), user)
		if errors.Is(err, store.ErrConflict) {
			continue // 昵称+tag 或占位邮箱撞车，重摇
		}
		break
	}
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	s.log.Printf("[audit] guest created user=%d ip=%s", user.ID, s.clientIP(r))
	s.issueTokens(w, r, user, dev, http.StatusCreated)
}

// randDigits 返回 n 位随机数字串（crypto/rand；前导零保留）。
func randDigits(n int) string {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	v, err := rand.Int(rand.Reader, max)
	if err != nil { // crypto/rand 不可用时退化为时间散列（仍保证非空）
		return fmt.Sprintf("%0*d", n, time.Now().UnixNano()%max.Int64())
	}
	return fmt.Sprintf("%0*d", n, v.Int64())
}

// randHex 返回 n 字节随机量的 hex 串（占位邮箱唯一性）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

// handleLogin POST /api/v1/auth/login（限流 5 次/min/IP）
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ok, err := s.limits.Allow(r.Context(), "login:"+s.clientIP(r), 5, time.Minute)
	if err != nil || !ok {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "登录过于频繁，请稍后再试")
		return
	}
	var req loginReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Email = normalizeEmail(req.Email)
	dev, okDev := deviceType(req.Device)
	if !okDev {
		writeErr(w, http.StatusBadRequest, "bad_device", "设备类型不合法（仅 desktop/mobile）")
		return
	}
	user, err := s.store.UserByEmail(r.Context(), req.Email)
	// 等时比较（#22）：用户不存在也走一次 bcrypt，消除「存在/不存在」时序差。
	hash := ""
	if err == nil {
		hash = user.PasswordHash
	}
	if err != nil || !auth.CheckPassword(hash, req.Password) {
		writeErr(w, http.StatusUnauthorized, "bad_credentials", "邮箱或密码错误")
		s.log.Printf("[audit] login failed email=%s ip=%s", req.Email, s.clientIP(r))
		return
	}
	s.log.Printf("[audit] login ok user=%d ip=%s", user.ID, s.clientIP(r))
	s.issueTokens(w, r, user, dev, http.StatusOK)
}

// handleRefresh POST /api/v1/auth/refresh（旋转刷新令牌）
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少 refresh_token")
		return
	}
	// 令牌自描述 uid/dev/sid（R1：<uid>.<dev>.<sid>.<rand>，旧格式回落 dev=desktop）。
	// 设备类型从令牌解析而非写死——P0-2：mobile 用户刷新时按 desktop 取键必败。
	uid, dev, sid, ok := auth.ParseRefreshToken(req.RefreshToken)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "refresh_token 不合法")
		return
	}
	// sid 与当前会话不符：另一会话已接管该设备槽位（session_replaced）。
	if s.sess != nil && sid != "" {
		curSid, err := s.sess.Get(r.Context(), uid, dev)
		if err == nil && curSid != "" && sid != curSid {
			w.Header().Set("X-LiteTalk-Auth", "session_replaced")
			writeErr(w, http.StatusUnauthorized, "session_replaced", "账号已在其他设备登录")
			return
		}
	}
	plain, err := s.refresh.Rotate(r.Context(), uid, req.RefreshToken)
	if err != nil {
		code := "unauthorized"
		if errors.Is(err, auth.ErrSessionReplayed) {
			code = "session_replayed"
			w.Header().Set("X-LiteTalk-Auth", "session_replayed")
		}
		writeErr(w, http.StatusUnauthorized, code, "refresh_token 无效或已吊销")
		return
	}
	user, err := s.store.UserByID(r.Context(), uid)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	access, err := s.jwt.SignAccess(user.ID, user.Nickname, user.Tag, sid, dev)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenPair{
		AccessToken:  access,
		RefreshToken: plain,
		ExpiresIn:    int64(auth.AccessTokenTTL / time.Second),
		User:         user,
	})
}

// handleLogout POST /api/v1/auth/logout
// 吊销前先校验令牌哈希与存储一致（P1）：否则伪造 "<uid>.x" 即可吊销他人 refresh。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req logoutReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// 设备类型同样从令牌解析（R1），不再写死 desktop。
	uid, dev, _, ok := auth.ParseRefreshToken(req.RefreshToken)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "refresh_token 不合法")
		return
	}
	if !s.refresh.Match(r.Context(), uid, req.RefreshToken) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "refresh_token 无效或已吊销")
		return
	}
	if err := s.refresh.Revoke(r.Context(), uid, req.RefreshToken); err != nil {
		s.log.Printf("[audit] logout 吊销失败 uid=%d err=%v", uid, err)
		writeErr(w, http.StatusInternalServerError, "internal", "服务器内部错误")
		return
	}
	// 删除该设备会话记录。
	if s.sess != nil {
		_ = s.sess.Delete(r.Context(), uid, dev)
	}
	// 登出即踢自己的 WS（同 uid+dev 的活动连接）；reason 空→login_elsewhere（契约 §0.11）。
	if s.hub != nil {
		s.hub.Kick(strconv.FormatInt(uid, 10), dev, "")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleEmailCode POST /api/v1/auth/email-code（限流 10 次/h/IP：防对批量邮箱轰炸发信）
func (s *Server) handleEmailCode(w http.ResponseWriter, r *http.Request) {
	if s.simpleMode() {
		writeErr(w, http.StatusBadRequest, "verification_not_required", "当前服务未启用邮箱验证，无需验证码")
		return
	}
	ok, err := s.limits.Allow(r.Context(), "emailcode:"+s.clientIP(r), 10, time.Hour)
	if err != nil || !ok {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "操作过于频繁，请稍后再试")
		return
	}
	var req emailCodeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Email = normalizeEmail(req.Email)
	if !validEmail(req.Email) {
		writeErr(w, http.StatusBadRequest, "bad_email", "邮箱格式不正确")
		return
	}
	if req.Purpose != auth.PurposeRegister && req.Purpose != auth.PurposeReset {
		writeErr(w, http.StatusBadRequest, "bad_purpose", "用途必须是 register 或 reset")
		return
	}
	// register：邮箱不应已注册；reset：邮箱必须已注册（防枚举差异统一提示）。
	_, lookupErr := s.store.UserByEmail(r.Context(), req.Email)
	if req.Purpose == auth.PurposeRegister && lookupErr == nil {
		writeErr(w, http.StatusConflict, "email_taken", "邮箱已注册")
		return
	}
	if req.Purpose == auth.PurposeReset && lookupErr != nil {
		writeErr(w, http.StatusNotFound, "not_found", "邮箱未注册")
		return
	}
	code, err := s.codes.Send(r.Context(), req.Purpose, req.Email)
	switch {
	case errors.Is(err, auth.ErrCodeTooFrequent):
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "发送过于频繁，请 60 秒后再试")
		return
	case errors.Is(err, auth.ErrCodeDailyLimit):
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "今日发送次数已达上限")
		return
	case err != nil:
		s.writeStoreErr(w, r, err)
		return
	}
	if err := s.mailer.SendCode(req.Email, req.Purpose, code); err != nil {
		s.log.Printf("[mailer] 发送失败 to=%s err=%v", req.Email, err)
	}
	s.log.Printf("[audit] email-code sent email=%s purpose=%s ip=%s", req.Email, req.Purpose, s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePasswordReset POST /api/v1/auth/password/reset
func (s *Server) handlePasswordReset(w http.ResponseWriter, r *http.Request) {
	// simple 模式没有邮件验证码可依赖，只凭邮箱+新密码即改密等于把账号送人（P0 S3），
	// 故该模式整体禁用找回密码；确有需求的部署应改用 full 模式。
	if s.simpleMode() {
		writeErr(w, http.StatusForbidden, "reset_unavailable", "当前服务端未开启邮件功能，无法找回密码")
		return
	}
	// 限流 10 次/h/IP：防对批量邮箱尝试爆破验证码。
	ok, err := s.limits.Allow(r.Context(), "reset:"+s.clientIP(r), 10, time.Hour)
	if err != nil || !ok {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "操作过于频繁，请稍后再试")
		return
	}
	var req passwordResetReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Email = normalizeEmail(req.Email)
	if !validPassword(req.Password) {
		writeErr(w, http.StatusBadRequest, "bad_password", "密码需 8–72 个字符")
		return
	}
	// full 模式必须过邮箱验证码。
	if err := s.codes.Verify(r.Context(), auth.PurposeReset, req.Email, req.Code); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_code", "验证码无效或已过期")
		return
	}
	user, err := s.store.UserByEmail(r.Context(), req.Email)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "邮箱未注册")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	user.PasswordHash = hash
	if err := s.store.UpdateUser(r.Context(), user); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	// 重置后吊销全部刷新令牌。
	_ = s.refresh.RevokeAll(r.Context(), user.ID)
	// 删除该用户全部设备会话记录。
	if s.sess != nil {
		_ = s.sess.DeleteAll(r.Context(), user.ID)
	}
	// 重置密码踢该用户全部设备 WS（契约 §0.11：dev 空=全部）。
	if s.hub != nil {
		s.hub.Kick(strconv.FormatInt(user.ID, 10), "", "password_reset")
	}
	s.log.Printf("[audit] password reset user=%d ip=%s", user.ID, s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// issueTokens 签发 access+refresh 并响应。dev=设备类型（desktop/mobile）。
func (s *Server) issueTokens(w http.ResponseWriter, r *http.Request, user *store.User, dev string, status int) {
	// 登录/注册/游客成功时覆盖会话（新 sid 即吊销旧会话）。
	// Upsert 失败必须 500：降级签发无 sid 的令牌会让 auth 中间件把登录态
	// 全部判成 session_replaced（PR #2 审查 P0-1），宁可不发令牌。
	if s.sess == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "会话服务未初始化")
		return
	}
	sid, err := s.sess.Upsert(r.Context(), user.ID, dev, s.clientIP(r))
	if err != nil {
		s.log.Printf("[session] upsert 失败 uid=%d dev=%s err=%v", user.ID, dev, err)
		writeErr(w, http.StatusInternalServerError, "internal", "创建会话失败")
		return
	}
	access, err := s.jwt.SignAccess(user.ID, user.Nickname, user.Tag, sid, dev)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	refresh, err := s.refresh.IssueWithUID(r.Context(), user.ID, dev, sid)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, status, tokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int64(auth.AccessTokenTTL / time.Second),
		User:         user,
	})
}

// normalizeEmail 邮箱小写去空白。
func normalizeEmail(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r != ' ' && r != '\t' {
			out = append(out, r)
		}
	}
	return strings.ToLower(string(out))
}
