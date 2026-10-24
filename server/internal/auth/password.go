// Package auth 账号安全：bcrypt 密码、JWT 访问令牌、Redis 刷新令牌旋转、邮箱验证码。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 按安全要点 §8 固定 12。仅测试可经 SetBcryptCostForTest 临时降低。
var bcryptCost = 12

// SetBcryptCostForTest 仅供测试在 TestMain 中调用一次：把 bcrypt cost 压到
// [bcrypt.MinCost, bcrypt.DefaultCost] 以缩短注册/登录夹具耗时；越界值忽略。
// 生产代码不得调用，运行期并发修改会与 HashPassword 形成数据竞争。
func SetBcryptCostForTest(cost int) {
	if cost < bcrypt.MinCost || cost > bcrypt.DefaultCost {
		return
	}
	bcryptCost = cost
}

// dummyHash 固定 bcrypt 哈希：游客/空哈希路径也走一次完整比较，消除空哈希早退的时序差（#22）。
// 故意保持 cost 12，不随测试降本——等时语义要按生产开销验证。
var dummyHash = []byte("$2a$12$UlbcvL1zfCj4bH0TrfoIaeFN9ykOog8EHv4Wx8rjYAnXGdDakbU.6")

// HashPassword 按 bcryptCost 哈希（生产为 12）。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	return string(b), err
}

// CheckPassword 校验密码。空哈希（游客）也走一次 bcrypt 比较再返回失败（等时）。
func CheckPassword(hash, plain string) bool {
	if hash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(plain))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ErrInvalidToken 令牌非法或过期。
var ErrInvalidToken = errors.New("invalid token")

// RandomToken 生成 n 字节随机串（hex 编码），用于刷新令牌。
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashToken 刷新令牌落库存哈希（SHA-256），明文只出现在响应/请求中。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RandomCode 生成 6 位数字验证码。
func RandomCode() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return fmt.Sprintf("%06d", n%1000000), nil
}
