package main

import (
	"flag"
	"fmt"

	"github.com/OpenALC/LiteTalk/client/core/api"
)

// cmdRegister 注册并直接落登录态（注册响应自带令牌对，等价登录）。
// simple 模式免验证码；full 模式需 --code（先用别的途径取验证码）。
func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	email := fs.String("email", "", "邮箱")
	password := fs.String("password", "", "密码")
	nickname := fs.String("nickname", "", "昵称")
	code := fs.String("code", "", "邮箱验证码（full 模式必填）")
	bootstrap := fs.String("bootstrap", "", "管理员邀请码（首个管理员用）")
	addr := fs.String("addr", "", "服务端基址")
	_ = fs.Parse(args)
	if *email == "" || *password == "" || *nickname == "" {
		return fmt.Errorf("register 需要 --email --password --nickname")
	}
	st, err := loadState()
	if err != nil {
		return err
	}
	base := resolveBase(*addr, st)
	c := newAPI(base, nil)
	r, err := c.Register(*nickname, *email, *password, *code, *bootstrap)
	if err != nil {
		return err
	}
	st.applyAuth(base, r)
	if err := saveState(st); err != nil {
		return err
	}
	fmt.Printf("ok register id=%s nick=%s tag=%s session_ok=%v\n",
		st.UserID, st.Nickname, st.Tag, r.SessionOK)
	return nil
}

// cmdLogin 邮箱密码登录；--guest 走游客一键进入（simple 模式）。
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	email := fs.String("email", "", "邮箱")
	password := fs.String("password", "", "密码")
	guest := fs.Bool("guest", false, "游客登录（simple 模式）")
	addr := fs.String("addr", "", "服务端基址")
	_ = fs.Parse(args)
	if !*guest && (*email == "" || *password == "") {
		return fmt.Errorf("login 需要 --email --password，或 --guest")
	}
	st, err := loadState()
	if err != nil {
		return err
	}
	base := resolveBase(*addr, st)
	c := newAPI(base, nil)
	var ar *api.AuthResult
	if *guest {
		ar, err = c.Guest()
	} else {
		ar, err = c.Login(*email, *password)
	}
	if err != nil {
		return err
	}
	st.applyAuth(base, ar)
	if err := saveState(st); err != nil {
		return err
	}
	fmt.Printf("ok login id=%s nick=%s tag=%s session_ok=%v\n",
		st.UserID, st.Nickname, st.Tag, ar.SessionOK)
	return nil
}

// cmdLogout 吊销 refresh 并清本地登录态。服务端不可达时本地照样清（退出优先）。
func cmdLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	addr := fs.String("addr", "", "服务端基址")
	_ = fs.Parse(args)
	st, err := loadState()
	if err != nil {
		return err
	}
	base := resolveBase(*addr, st)
	if st.RefreshToken != "" {
		if err := newAPI(base, st).Logout(st.RefreshToken); err != nil {
			fmt.Printf("warn logout 远端吊销失败（本地仍清除）: %v\n", err)
		}
	}
	if err := clearState(); err != nil {
		return err
	}
	fmt.Println("ok logout")
	return nil
}

// cmdWhoami 打印当前登录用户（对外不展示 email——隐私约束）。
func cmdWhoami(args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ExitOnError)
	addr := fs.String("addr", "", "服务端基址")
	_ = fs.Parse(args)
	st, err := loadState()
	if err != nil {
		return err
	}
	base := resolveBase(*addr, st)
	me, err := newAPI(base, st).Me()
	if err != nil {
		return err
	}
	fmt.Printf("ok me id=%s nick=%s tag=%s guest=%v\n",
		me.ID.String(), me.Nickname, me.Tag, me.IsGuest)
	return nil
}
