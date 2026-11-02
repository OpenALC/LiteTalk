package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// releaseOrderServer 按给定顺序返回 release 列表，用来固定"列表顺序"这一变量。
// 真实 GitHub 按发布时间倒序返回，补发旧版本会排在真正最新版之前。
func releaseOrderServer(t *testing.T, tags []ghRelease) *GitHubClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, tags)
	}))
	t.Cleanup(srv.Close)
	return &GitHubClient{APIBase: srv.URL, Repo: "OpenALC/LiteTalk", Client: srv.Client(), Log: testLogger()}
}

// TestResolveLatestPicksHighestSemver 列表顺序打乱时仍取 semver 最高版本。
func TestResolveLatestPicksHighestSemver(t *testing.T) {
	// 最旧 → 最新交错的顺序：任何"取第一条"的实现都会选错
	rels := []ghRelease{
		{TagName: "v1.5.1-alpha", Prerelease: true},
		{TagName: "v1.10.0-alpha", Prerelease: true},
		{TagName: "v1.9.3-alpha", Prerelease: true},
		{TagName: "v1.6.0-alpha", Prerelease: true},
	}
	g := releaseOrderServer(t, rels)
	got, err := g.ResolveLatest(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	// v1.10.0 > v1.9.3 说明按数值比较而非字符串比较
	if got != "v1.10.0-alpha" {
		t.Fatalf("ResolveLatest = %q, want v1.10.0-alpha", got)
	}

	// 再加一个更高的正式版，通道放行时它应胜出
	rels2 := append(rels, ghRelease{TagName: "v1.11.0", Prerelease: false})
	g2 := releaseOrderServer(t, rels2)
	if got, err := g2.ResolveLatest(context.Background(), true); err != nil || got != "v1.11.0" {
		t.Fatalf("ResolveLatest = %q err=%v，want v1.11.0", got, err)
	}
}

// TestResolveLatestPrereleaseFilteredBeforeOrder 未放行预发布时，先过滤再取最高：
// 最高的 alpha 被过滤后应回落到正式版，而不是报"没有 release"。
func TestResolveLatestPrereleaseFilteredBeforeOrder(t *testing.T) {
	g := releaseOrderServer(t, []ghRelease{
		{TagName: "v2.0.0-alpha", Prerelease: true},
		{TagName: "v1.6.2", Prerelease: false},
		{TagName: "v1.6.0", Prerelease: false},
	})
	got, err := g.ResolveLatest(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.6.2" {
		t.Fatalf("ResolveLatest = %q, want v1.6.2", got)
	}

	// 全部是预发布且未放行：报错
	g2 := releaseOrderServer(t, []ghRelease{{TagName: "v2.0.0-alpha", Prerelease: true}})
	if _, err := g2.ResolveLatest(context.Background(), false); err == nil {
		t.Fatal("全预发布且未放行应报错")
	}
}

// TestResolveLatestIgnoresUnparsableTags 白名单外的 tag 不参与比较。
func TestResolveLatestIgnoresUnparsableTags(t *testing.T) {
	g := releaseOrderServer(t, []ghRelease{
		{TagName: "nightly"},
		{TagName: "v1.2.3-rc.1"},
		{TagName: "v1.2.3"},
	})
	got, err := g.ResolveLatest(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.2.3" {
		t.Fatalf("ResolveLatest = %q, want v1.2.3", got)
	}
}
