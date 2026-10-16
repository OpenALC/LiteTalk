//go:build windows

// Package config 的 Windows DPAPI 加密实现（问题 6a）：
// CryptProtectData / CryptUnprotectData（user scope，syscall 绑定 crypt32，零新依赖）。
package config

import (
	"encoding/base64"
	"errors"
	"syscall"
	"unsafe"
)

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	procProtect   = crypt32.NewProc("CryptProtectData")
	procUnprotect = crypt32.NewProc("CryptUnprotectData")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree = kernel32.NewProc("LocalFree")
)

// dataBlob 对应 Win32 DATA_BLOB。
type dataBlob struct {
	cbData uint32
	pbData *byte
}

const cryptProtectUIForbidden = 0x1

// blobFromBytes 把 Go 切片包成 DATA_BLOB（空切片返回零值 blob）。
func blobFromBytes(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

// protectString DPAPI 加密（user scope），返回 base64 密文。
func protectString(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	in := blobFromBytes([]byte(plain))
	var out dataBlob
	r, _, err := procProtect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", errors.New("dpapi protect: " + err.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	enc := make([]byte, out.cbData)
	copy(enc, unsafe.Slice(out.pbData, out.cbData))
	return base64.StdEncoding.EncodeToString(enc), nil
}

// unprotectString DPAPI 解密 base64 密文（同一用户上下文才能解）。
func unprotectString(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	in := blobFromBytes(raw)
	var out dataBlob
	r, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", errors.New("dpapi unprotect: " + err.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	plain := make([]byte, out.cbData)
	copy(plain, unsafe.Slice(out.pbData, out.cbData))
	return string(plain), nil
}
