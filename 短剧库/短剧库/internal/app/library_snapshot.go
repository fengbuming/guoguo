package app

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 列表响应磁盘快照
//
// 整库列表响应实测 27.4 MB（27,700 余部），每次请求都要逐条编码并复制一份条目切片。
// 这里把「同一状态下的完整响应体」预渲染成 gzip 文件落到 data/library/snapshot/，
// 请求期只做 http.ServeContent（sendfile），常驻内存与 CPU 都接近零；
// 客户端带 If-None-Match 复访时直接 304，连流量也只剩一个头部。
//
// 快照键由「头字段 + 条目数 + 站源作用域」共同决定：只要有任何一个进度字段变化
//（例如 loading/loadingPhase），键就会变，从而自动重渲染，不会返回过期状态。
const librarySnapshotKeep = 6

// librarySnapshotDir 返回快照落盘目录；未显式配置 data 目录时退到系统临时目录，
// 避免测试或临时实例把文件写进工作目录。
func (app *UIApp) librarySnapshotDir() string {
	if dir := strings.TrimSpace(app.cfg.dataDir); dir != "" {
		return filepath.Join(dir, "library", "snapshot")
	}
	return filepath.Join(os.TempDir(), "juku-library-snapshot")
}

func librarySnapshotPath(dir, key string) string {
	return filepath.Join(dir, key+".json.gz")
}

// readyLibrarySnapshot 返回已经渲染好的快照路径。
func readyLibrarySnapshot(dir, key string) (string, bool) {
	path := librarySnapshotPath(dir, key)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", false
	}
	_ = os.Chtimes(path, time.Now(), time.Now())
	return path, true
}

func renderLibrarySnapshot(dir, key string, fields map[string]any, entries []dramaEntry) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(dir, "snapshot-*.tmp")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	done := false
	defer func() {
		if !done {
			_ = temp.Close()
			_ = os.Remove(name)
		}
	}()
	compressed, err := gzip.NewWriterLevel(temp, gzip.DefaultCompression)
	if err != nil {
		return "", err
	}
	chunk := writeJSONChunkWriter{w: compressed}
	writeLibraryObjectJSON(&chunk, fields, entries)
	chunk.flush()
	if err := compressed.Close(); err != nil {
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	final := librarySnapshotPath(dir, key)
	if err := os.Rename(name, final); err != nil {
		return "", err
	}
	done = true
	pruneLibrarySnapshots(dir, key)
	return final, nil
}

// pruneLibrarySnapshots 只保留最近写入的若干份快照，避免剧库反复更新时堆满磁盘。
func pruneLibrarySnapshots(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		name string
		mod  time.Time
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json.gz") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{name: entry.Name(), mod: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	kept := 1
	for _, current := range items {
		if current.name == keep+".json.gz" {
			continue
		}
		kept++
		if kept > librarySnapshotKeep {
			_ = os.Remove(filepath.Join(dir, current.name))
		}
	}
}

// librarySnapshotScopeKey 生成站源作用域签名：不同账号可见的源不同，快照不能互相复用。
func librarySnapshotScopeKey(ctx context.Context) string {
	scope := sourceScope(ctx)
	if scope == nil {
		return "all"
	}
	sources := append([]string(nil), scope.Sources...)
	sort.Strings(sources)
	prefix := "open:"
	if scope.OnlineOnly {
		prefix = "online:"
	}
	return prefix + strings.Join(sources, ",")
}

// librarySnapshotKey 把快照头字段、条目数与作用域揉成一个稳定键。
func librarySnapshotKey(fields map[string]any, count int, scope string) string {
	hash := sha256.New()
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		body, err := json.Marshal(fields[key])
		if err != nil {
			body = nil
		}
		hash.Write([]byte(key))
		hash.Write([]byte{0})
		hash.Write(body)
		hash.Write([]byte{1})
	}
	hash.Write([]byte{2})
	hash.Write([]byte(strconv.Itoa(count)))
	hash.Write([]byte{2})
	hash.Write([]byte(scope))
	return hex.EncodeToString(hash.Sum(nil))[:32]
}

func requestAcceptsGzip(request *http.Request) bool {
	for _, part := range strings.Split(request.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		return !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0")
	}
	return false
}

// serveLibrarySnapshot 优先用磁盘快照应对整库列表请求，任何异常都回落到流式编码。
func (app *UIApp) serveLibrarySnapshot(writer http.ResponseWriter, request *http.Request, fields map[string]any, entries []dramaEntry) {
	if entries == nil || request.Method != http.MethodGet && request.Method != http.MethodHead || !requestAcceptsGzip(request) {
		writeLibrarySnapshot(writer, http.StatusOK, fields, entries)
		return
	}
	dir := app.librarySnapshotDir()
	key := librarySnapshotKey(fields, len(entries), librarySnapshotScopeKey(request.Context()))
	path, ok := readyLibrarySnapshot(dir, key)
	if !ok {
		rendered, err := renderLibrarySnapshot(dir, key, fields, entries)
		if err != nil {
			writeLibrarySnapshot(writer, http.StatusOK, fields, entries)
			return
		}
		path = rendered
	}
	file, err := os.Open(path)
	if err != nil {
		writeLibrarySnapshot(writer, http.StatusOK, fields, entries)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeLibrarySnapshot(writer, http.StatusOK, fields, entries)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Content-Encoding", "gzip")
	writer.Header().Set("Vary", "Accept-Encoding")
	writer.Header().Set("Cache-Control", "private, no-cache")
	writer.Header().Set("ETag", `"`+key+`"`)
	http.ServeContent(writer, request, "library.json", info.ModTime(), file)
}

// writeLibrarySnapshotJSON 把 {头字段..., "data":[条目...]} 写到任意 writer。
func writeLibrarySnapshotJSON(writer io.Writer, fields map[string]any, entries []dramaEntry) {
	chunk := writeJSONChunkWriter{w: writer}
	writeLibraryObjectJSON(&chunk, fields, entries)
	chunk.flush()
}

// writeLibraryObjectJSON 逐条编码 + 分块写出，避免 json.Encoder 先整体缓冲造成的额外内存峰值。
// 头字段按键排序输出，保证同一份状态渲染出的字节完全一致（快照键依赖这一点）。
func writeLibraryObjectJSON(chunk *writeJSONChunkWriter, fields map[string]any, entries []dramaEntry) {
	chunk.byte('{')
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		body, err := json.Marshal(fields[key])
		if err != nil {
			continue
		}
		chunk.string(strconv.Quote(key))
		chunk.byte(':')
		chunk.bytes(body)
		chunk.byte(',')
	}
	chunk.string(`"data":[`)
	for index := range entries {
		if index > 0 {
			chunk.byte(',')
		}
		body, err := json.Marshal(&entries[index])
		if err != nil {
			continue
		}
		chunk.bytes(body)
	}
	chunk.string(`]}`)
}
