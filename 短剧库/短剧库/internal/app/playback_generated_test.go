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
