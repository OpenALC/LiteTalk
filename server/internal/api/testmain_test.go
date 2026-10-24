package api

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
)

// TestMain 把 bcrypt cost 降到最小值：本包大量用例走注册/登录夹具，cost 12 在 -race 下
// 会把整包推到 CI 的 10 分钟上限。业务断言不依赖 cost；dummyHash 仍是 cost 12，
// 空哈希等时路径的语义不受影响。
func TestMain(m *testing.M) {
	auth.SetBcryptCostForTest(bcrypt.MinCost)
	os.Exit(m.Run())
}
