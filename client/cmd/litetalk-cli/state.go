// litetalk-cli 是无界面客户端（#34）：复用 core/api + core/voice 跑通
// 注册/登录/建频道/入房通话，供服务器/CI/无界面环境验证 core 逻辑。
// 不 import desktop/、wails——core 边界测试（core_boundary_test.go）不拦 cmd，
// 但本工具自身也不得反向依赖桌面壳件。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenALC/LiteTalk/client/core/api"
)

// State CLI 本地登录态（独立于桌面端 config.json，互不干扰）。
type State struct {
	BaseURL      string `json:"base_url"` // 最近使用的 REST 基址
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	Nickname     string `json:"nickname"`
	Tag          string `json:"tag"`
	Email        string `json:"email,omitempty"`
}

// statePath 状态文件位置：LITETALK_CLI_STATE 显式指定 > OS 用户配置目录/litetalk-cli/state.json。
// 显式路径给同机多实例（机器人/压测）隔离登录态用。
func statePath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("LITETALK_CLI_STATE")); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "litetalk-cli", "state.json"), nil
}

// readStateFile 读状态文件。不存在返回空态不算错（未登录的正常状态）。
func readStateFile(p string) (*State, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	st := &State{}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	return st, nil
}

// marshalState 序列化状态（缩进便于人读）。
func marshalState(st *State) ([]byte, error) {
	return json.MarshalIndent(st, "", "  ")
}

// loadState 读取登录态。文件不存在返回空态不算错（未登录的正常状态）。
func loadState() (*State, error) {
	p, err := statePath()
	if err != nil {
		return nil, err
	}
	return readStateFile(p)
}

// saveState 落盘登录态。0600：内含 access/refresh 令牌。
func saveState(st *State) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := marshalState(st)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// applyAuth 把登录/注册结果写进状态并落盘。
func (st *State) applyAuth(base string, r *api.AuthResult) {
	st.BaseURL = base
	st.AccessToken = r.AccessToken
	st.RefreshToken = r.RefreshToken
	st.UserID = r.User.ID.String()
	st.Nickname = r.User.Nickname
	st.Tag = r.User.Tag
	st.Email = r.User.Email
}

// clearState 删除登录态文件（logout 用）。
func clearState() error {
	p, err := statePath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
