package app

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func snapshotTestApp(t *testing.T) *UIApp {
	t.Helper()
	app := &UIApp{
		cfg:     Config{dataDir: t.TempDir()},
		library: newLibraryStoreWith([]Drama{
			{ID: "hongguo:1", Source: sourceHongguo, Title: "甲剧", Desc: "第一部简介", Cover: coverAddressFixture},
			{ID: "hongguo:2", Source: sourceHongguo, Title: "乙剧", Desc: "第二部简介"},
			{ID: "huangdou:3", Source: sourceHuangdou, Title: "丙剧", Desc: "第三部简介"},
		}),
		selected:        map[string]bool{},
		librarySources:  map[string]librarySourceState{},
		libraryProviders: map[string]providerCatalogCursor{},
		metadataPending: map[string]bool{},
	}
	return app
}

func snapshotRequest(app *UIApp, gzipped bool, etag string) *httptest.ResponseRecorder {
	app.mu.Lock()
	fields, entries := app.librarySnapshotLocked(0)
	app.mu.Unlock()
	request := httptest.NewRequest(http.MethodGet, "/api/ui/dramas", nil)
	if gzipped {
		request.Header.Set("Accept-Encoding", "gzip")
	}
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	recorder := httptest.NewRecorder()
	app.serveLibrarySnapshot(recorder, request, fields, entries)
	return recorder
}

func decodeSnapshotBody(t *testing.T, recorder *httptest.ResponseRecorder) []byte {
	t.Helper()
	body := recorder.Body.Bytes()
	if recorder.Header().Get("Content-Encoding") != "gzip" {
		return body
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("快照不是合法 gzip: %v", err)
	}
	defer reader.Close()
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("解压快照失败: %v", err)
	}
	return plain
}

// 快照必须与流式编码产出完全一致的内容，只是换成磁盘 gzip 文件。
func TestLibrarySnapshotMatchesStreamedEncoding(t *testing.T) {
	app := snapshotTestApp(t)
	gzipped := snapshotRequest(app, true, "")
	if gzipped.Code != http.StatusOK || gzipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip 快照响应异常: %d %q", gzipped.Code, gzipped.Header().Get("Content-Encoding"))
	}
	if gzipped.Body.Len() == 0 || gzipped.Header().Get("ETag") == "" {
		t.Fatal("快照缺少实体或 ETag")
	}
	plain := snapshotRequest(app, false, "")
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("客户端不支持 gzip 时不应压缩")
	}
	var fromSnapshot, fromStream map[string]any
	if err := json.Unmarshal(decodeSnapshotBody(t, gzipped), &fromSnapshot); err != nil {
		t.Fatalf("解析快照失败: %v", err)
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &fromStream); err != nil {
		t.Fatalf("解析流式响应失败: %v", err)
	}
	left, _ := json.Marshal(fromSnapshot)
	right, _ := json.Marshal(fromStream)
	if !bytes.Equal(left, right) {
		t.Fatalf("快照与流式响应不一致:\n%s\n%s", left, right)
	}
	rows, _ := fromSnapshot["data"].([]any)
	if len(rows) != 3 {
		t.Fatalf("条目数应为 3，实际 %d", len(rows))
	}
}

// 同一状态复访应命中磁盘快照并返回 304，不再重渲染。
func TestLibrarySnapshotRevalidatesAndRendersOnChange(t *testing.T) {
	app := snapshotTestApp(t)
	first := snapshotRequest(app, true, "")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("缺少 ETag")
	}
	dir := app.librarySnapshotDir()
	matches, err := filepath.Glob(filepath.Join(dir, "*.json.gz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("应落盘 1 份快照，实际 %d (%v)", len(matches), err)
	}
	info, err := os.Stat(matches[0])
	if err != nil || info.Size() == 0 {
		t.Fatalf("快照文件异常: %v", err)
	}
	second := snapshotRequest(app, true, etag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("带 If-None-Match 应返回 304，实际 %d", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 不应带实体，实际 %d 字节", second.Body.Len())
	}
	app.mutateDramaAt(0, func(drama *Drama) { drama.Title = "甲剧（改名）" })
	app.libraryRevision++
	third := snapshotRequest(app, true, etag)
	if third.Code != http.StatusOK || third.Header().Get("ETag") == etag {
		t.Fatalf("剧库变化后应重渲染，实际 %d %q", third.Code, third.Header().Get("ETag"))
	}
	if !bytes.Contains(decodeSnapshotBody(t, third), []byte("甲剧（改名）")) {
		t.Fatal("重渲染的快照没有包含新标题")
	}
}

// 不同站源作用域不能共用同一份快照。
func TestLibrarySnapshotKeySeparatesSourceScopes(t *testing.T) {
	fields := map[string]any{"revision": uint64(7), "total": 3, "loading": false}
	open := librarySnapshotScopeKey(withSourceScope(t.Context(), accountRecord{Admin: true}))
	member := librarySnapshotScopeKey(withSourceScope(t.Context(), accountRecord{Sources: []string{sourceHongguo}}))
	if open == member {
		t.Fatal("管理员与受限账号的作用域签名不能相同")
	}
	if librarySnapshotKey(fields, 3, open) == librarySnapshotKey(fields, 3, member) {
		t.Fatal("不同站源作用域复用了同一快照键")
	}
	if librarySnapshotKey(fields, 3, open) != librarySnapshotKey(fields, 3, open) {
		t.Fatal("同一状态必须得到同一快照键")
	}
}

// 快照只保留最近若干份，避免剧库反复更新时堆满磁盘。
func TestLibrarySnapshotPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	for index := 0; index < librarySnapshotKeep+3; index++ {
		name := filepath.Join(dir, "key"+string(rune('a'+index))+".json.gz")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneLibrarySnapshots(dir, "keep")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > librarySnapshotKeep {
		t.Fatalf("快照文件未清理，剩 %d 份", len(entries))
	}
}
