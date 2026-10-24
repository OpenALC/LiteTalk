#!/usr/bin/env bash
# 契约模糊测试（#30）：schemathesis 按 api/openapi.yaml 对临时服务端跑一轮 fuzz。
# 断言面固定「无 5xx + 响应形状符合契约」（§7）；不启用 status_code_conformance——
# 限流 429 是合法业务响应，不能当契约违约。固定 seed + 确定性生成，CI 可重复。
# 临时目录 + 随机/关闭媒体端口；REST 契约面不需要 SFU/TURN。
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)" # server/api/tests → 仓库根
cd "$root"

port="${LITETALK_FUZZ_PORT:-18777}"
# 数据/报告目录：CI 用固定路径挂 artifact，本地默认临时目录。
data="${LITETALK_FUZZ_DIR:-$(mktemp -d)}"
mkdir -p "$data"
srv_pid=""
cleanup() {
  if [ -n "$srv_pid" ]; then
    kill "$srv_pid" 2>/dev/null || true
    wait "$srv_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT

export LITETALK_ADDR="127.0.0.1:${port}"
export LITETALK_DATA_DIR="$data"
export LITETALK_JWT_SECRET="fuzz-$(head -c 24 /dev/urandom | base64 | tr -d '\n')"
export LITETALK_DEV_AUTH=1 # 仅 dev 构建允许（go build 不注入版本号即 dev）
export LITETALK_AUTH_MODE=simple
export LITETALK_TURN_PORT=0 # 关内置 TURN
# #77：rtc/credentials 已收编契约并会被 fuzz；给临时服务端一个 TURN 密钥，
# 让端点返回 200 而不是声明过的 503——schemathesis 的 not_a_server_error
# 不豁免 5xx，关掉 TURN 会让契约检查因合法的 503 失败。
export LITETALK_TURN_SECRET="fuzz-turn-$(head -c 24 /dev/urandom | base64 | tr -d '\n')"
export LITETALK_SFU_UDP=0  # 随机端口，不占 7845
export LITETALK_SFU_TCP=0  # 不启 ICE-TCP
export LITETALK_SFU_IP=127.0.0.1
# #42：链接预览默认开启会按 fuzz 生成的 url 真发外网请求——契约测试不依赖外网，
# 关掉后端点走 404 no_preview（契约内响应），抓取逻辑由 linkpreview 包单测覆盖。
export LITETALK_LINK_PREVIEW=off

echo "== 起临时服务端 127.0.0.1:${port}（数据目录 ${data}）"
(cd server && go build -o "$data/server" ./cmd/server)
"$data/server" >"$data/server.log" 2>&1 &
srv_pid=$!

ready=""
for _ in $(seq 1 90); do
  if curl -sf "http://127.0.0.1:${port}/api/v1/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$srv_pid" 2>/dev/null; then
    break
  fi
  sleep 1
done
if [ -z "$ready" ]; then
  echo "::error::服务端未就绪（提前退出或 90s 超时）"
  tail -n 50 "$data/server.log" || true
  exit 1
fi

# REST 鉴权是 JWT：simple 模式游客登录取令牌注入 fuzz（dev-token 只兜底 WS 面）。
# 令牌注入后 fuzz 会打到业务分支；登出/换会话类操作可能中途废掉令牌，
# 后续请求退化为 401——401 是契约内响应，不算违约。
# X-LiteTalk-Client 是 #31 的强制升级门：取令牌与 fuzz 请求都带 dev，
# 否则 /auth/* 一律 426，业务分支完全测不到（426 仍在契约里声明）。
resp="$(curl -sf -X POST "http://127.0.0.1:${port}/api/v1/auth/guest" \
  -H 'Content-Type: application/json' -H 'X-LiteTalk-Client: dev' -d '{}' || true)"
token="$(printf '%s' "$resp" | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
if [ -z "$token" ]; then
  echo "::error::游客登录未取得 access_token：${resp}"
  tail -n 50 "$data/server.log" || true
  exit 1
fi

echo "== schemathesis fuzz（seed=42，确定性生成，断言无 5xx + 形状符合契约）"
schemathesis run api/openapi.yaml \
  --url "http://127.0.0.1:${port}" \
  --header "Authorization: Bearer ${token}" \
  --header "X-LiteTalk-Client: dev" \
  --checks not_a_server_error,response_schema_conformance \
  --phases examples,coverage,fuzzing \
  --mode all \
  --seed 42 \
  --generation-deterministic \
  --max-examples 25 \
  --suppress-health-check too_slow \
  --request-timeout 15 \
  --max-time 600 \
  --report-junit-path "$data/schemathesis-junit.xml"
echo "== 契约模糊测试通过（报告 $data）"
