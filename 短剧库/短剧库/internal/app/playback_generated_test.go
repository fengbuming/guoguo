package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func playlistFixture(t *testing.T) *playbackGenerated {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	return &playbackGenerated{
		ctx: ctx, cancel: cancel, directory: t.TempDir(), base: "/media/",
		files: map[string]bool{}, done: make(chan struct{}),
	}
}

func writePlaylistFixture(t *testing.T, generated *playbackGenerated, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(generated.directory, "index.m3u8"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const playlistHeaderOnly = "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:5\n"

const playlistWithSegment = playlistHeaderOnly + "#EXTINF:4.0,\n000000.m4s\n"

// 首个分片还没出来时，清单请求应该等一会儿而不是把空清单丢给播放器。
func TestPlaybackGeneratedPlaylistWaitsForFirstSegment(t *testing.T) {
	generated := playlistFixture(t)
	defer generated.cancel()
	writePlaylistFixture(t, generated, playlistHeaderOnly)
	go func() {
		time.Sleep(250 * time.Millisecond)
		writePlaylistFixture(t, generated, playlistWithSegment)
	}()
	recorder := httptest.NewRecorder()
	started := time.Now()
	generated.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/index.m3u8", nil))
	elapsed := time.Since(started)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("清单在首片出现前就返回了（%v）", elapsed)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "000000.m4s") || !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Fatalf("返回的清单不可播或没标成 EVENT：%q", body)
	}
	if strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Fatal("还在生成时不能给 ENDLIST")
	}
}

// 等到超时仍没有分片：退回最小合法清单，让播放器按 live 规则重试。
func TestPlaybackGeneratedPlaylistFallsBackWhenCancelled(t *testing.T) {
	generated := playlistFixture(t)
	writePlaylistFixture(t, generated, playlistHeaderOnly)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/media/index.m3u8", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	started := time.Now()
	generated.ServeHTTP(recorder, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("请求已取消却等了 %v", elapsed)
	}
	if body := recorder.Body.String(); !strings.HasPrefix(body, "#EXTM3U") || !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Fatalf("兜底清单不合法：%q", body)
	}
}

// 预生成的产物要先按空 base 生成，切集前再换成真实 run 的地址。
func TestPlaybackGeneratedAdoptRewritesAssetURLs(t *testing.T) {
	generated := playlistFixture(t)
	defer generated.cancel()
	writePlaylistFixture(t, generated, playlistWithSegment)

	recorder := httptest.NewRecorder()
	generated.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/index.m3u8", nil))
	if body := recorder.Body.String(); !strings.Contains(body, "/media/000000.m4s") {
		t.Fatalf("初始地址不对：%q", body)
	}

	generated.adopt("/api/ui/playback/media/9/4/", "v=12")
	recorder = httptest.NewRecorder()
	generated.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/ui/playback/media/9/4/index.m3u8", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, "/api/ui/playback/media/9/4/000000.m4s?v=12") {
		t.Fatalf("接管后地址没换：%q", body)
	}
}

// 交出去的产物不能被预取收尾一起关掉；没交出去的必须由预取关掉。
func TestPlaybackPrefetchReleasesOnlyUnownedGenerated(t *testing.T) {
	taken := playlistFixture(t)
	cache := newPlaybackPrefetch(2, 1)
	cache.generated = taken
	if cache.takeGenerated() != taken {
		t.Fatal("takeGenerated 没交回产物")
	}
	cache.cancel()
	if taken.ctx.Err() != nil {
		t.Fatal("已交给前台的产物被预取关掉了")
	}

	owned := playlistFixture(t)
	other := newPlaybackPrefetch(3, 1)
	other.generated = owned
	other.cancel()
	if owned.ctx.Err() == nil {
		t.Fatal("没人接手的产物应该在预取收尾时关闭")
	}
}

// 分片文件按名字放行（渐进阶段 files 还没登记）。
func TestPlaybackGeneratedServesSegmentByName(t *testing.T) {
	generated := playlistFixture(t)
	defer generated.cancel()
	if err := os.WriteFile(filepath.Join(generated.directory, "000003.m4s"), []byte("stypmsdh"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	generated.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/000003.m4s", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "stypmsdh" {
		t.Fatalf("分片未按名字放行：%d %q", recorder.Code, recorder.Body.String())
	}
	denied := httptest.NewRecorder()
	generated.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/media/../escape.m4s", nil))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("非法分片名应 404，实际 %d", denied.Code)
	}
}
