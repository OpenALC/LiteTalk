package main

import (
	"flag"
	"fmt"
	"strconv"
)

// cmdServer 服务器子命令：create / list / invite / join。
func cmdServer(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("server 需要子命令：create | list | invite | join")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("server create", flag.ExitOnError)
		name := fs.String("name", "", "服务器名")
		addr := fs.String("addr", "", "服务端基址")
		_ = fs.Parse(args[1:])
		if *name == "" {
			return fmt.Errorf("server create 需要 --name")
		}
		c, _, err := authedClient(*addr)
		if err != nil {
			return err
		}
		srv, err := c.CreateServer(*name)
		if err != nil {
			return err
		}
		fmt.Printf("ok server create id=%s name=%s\n", srv.ID.String(), srv.Name)
		return nil
	case "list":
		fs := flag.NewFlagSet("server list", flag.ExitOnError)
		addr := fs.String("addr", "", "服务端基址")
		_ = fs.Parse(args[1:])
		c, _, err := authedClient(*addr)
		if err != nil {
			return err
		}
		list, err := c.ListServers()
		if err != nil {
			return err
		}
		for _, s := range list {
			fmt.Printf("server id=%s name=%s owner=%s\n", s.ID.String(), s.Name, s.OwnerID.String())
		}
		fmt.Printf("ok server list n=%d\n", len(list))
		return nil
	case "invite":
		fs := flag.NewFlagSet("server invite", flag.ExitOnError)
		id := fs.String("server", "", "服务器 ID")
		addr := fs.String("addr", "", "服务端基址")
		_ = fs.Parse(args[1:])
		if *id == "" {
			return fmt.Errorf("server invite 需要 --server")
		}
		c, _, err := authedClient(*addr)
		if err != nil {
			return err
		}
		code, err := c.CreateInvite(*id)
		if err != nil {
			return err
		}
		fmt.Printf("ok server invite code=%s\n", code)
		return nil
	case "join":
		fs := flag.NewFlagSet("server join", flag.ExitOnError)
		code := fs.String("code", "", "邀请码")
		addr := fs.String("addr", "", "服务端基址")
		_ = fs.Parse(args[1:])
		if *code == "" {
			return fmt.Errorf("server join 需要 --code")
		}
		c, _, err := authedClient(*addr)
		if err != nil {
			return err
		}
		srv, err := c.JoinServer(*code)
		if err != nil {
			return err
		}
		fmt.Printf("ok server join id=%s name=%s\n", srv.ID.String(), srv.Name)
		return nil
	default:
		return fmt.Errorf("未知 server 子命令 %q", args[0])
	}
}

// cmdChannel 频道子命令：create / list。
func cmdChannel(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("channel 需要子命令：create | list")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("channel create", flag.ExitOnError)
		server := fs.String("server", "", "服务器 ID")
		name := fs.String("name", "", "频道名")
		typ := fs.String("type", "voice", "频道类型 voice|text")
		addr := fs.String("addr", "", "服务端基址")
		_ = fs.Parse(args[1:])
		if *server == "" || *name == "" {
			return fmt.Errorf("channel create 需要 --server --name")
		}
		c, _, err := authedClient(*addr)
		if err != nil {
			return err
		}
		ch, err := c.CreateChannel(*server, *name, *typ, "")
		if err != nil {
			return err
		}
		fmt.Printf("ok channel create id=%s name=%s type=%s\n", ch.ID.String(), ch.Name, ch.Type)
		return nil
	case "list":
		fs := flag.NewFlagSet("channel list", flag.ExitOnError)
		server := fs.String("server", "", "服务器 ID")
		addr := fs.String("addr", "", "服务端基址")
		_ = fs.Parse(args[1:])
		if *server == "" {
			return fmt.Errorf("channel list 需要 --server")
		}
		c, _, err := authedClient(*addr)
		if err != nil {
			return err
		}
		sc, err := c.ListChannels(*server)
		if err != nil {
			return err
		}
		for _, ch := range sc.Channels {
			fmt.Printf("channel id=%s name=%s type=%s pos=%s\n",
				ch.ID.String(), ch.Name, ch.Type, strconv.Itoa(ch.Position))
		}
		fmt.Printf("ok channel list n=%d\n", len(sc.Channels))
		return nil
	default:
		return fmt.Errorf("未知 channel 子命令 %q", args[0])
	}
}
