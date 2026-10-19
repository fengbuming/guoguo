package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"
)

// TestLibraryMemoryProfile 用真实剧库文件测量内存占用，作为磁盘缓存改造的基线。
// 仅在设置 JUKU_LIBRARY_PROFILE 时才运行，避免影响常规测试：
//
//	JUKU_LIBRARY_PROFILE=D:/AI/guoguo/短剧库/短剧库/data/library.json go test ./internal/app/ -run TestLibraryMemoryProfile -v
func TestLibraryMemoryProfile(t *testing.T) {
	path := os.Getenv("JUKU_LIBRARY_PROFILE")
	if path == "" {
		t.Skip("未设置 JUKU_LIBRARY_PROFILE，跳过剧库内存测量")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取剧库失败: %v", err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	var cache libraryCache
	if err := json.Unmarshal(body, &cache); err != nil {
		t.Fatalf("解析剧库失败: %v", err)
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(cache)

	heapUsed := after.HeapAlloc - before.HeapAlloc
	count := len(cache.Dramas)
	var perRecord float64
	if count > 0 {
		perRecord = float64(heapUsed) / float64(count)
	}
	t.Logf("剧库文件: %.1f MiB, 剧集: %d 部", float64(len(body))/(1<<20), count)
	t.Logf("解析后堆内存: %.1f MiB（放大 %.2fx 文件大小）", float64(heapUsed)/(1<<20), float64(heapUsed)/float64(len(body)))
	t.Logf("单条 Drama 平均: %.0f 字节", perRecord)
	t.Logf("Drama 结构体静态大小: %d 字节（含 22 个 any 字段与 21 个字符串头）", unsafe.Sizeof(Drama{}))
	if count > 0 {
		t.Logf("仅结构体开销: %.1f MiB", float64(count)*float64(unsafe.Sizeof(Drama{}))/(1<<20))
	}
	// 结构体内各字段的静态大小拆解，便于定位改造收益
	t.Logf("字段: VIP=%d Source=%d Cover(any)=%d Episodes(any)=%d SortMetadata=%d",
		unsafe.Sizeof(Drama{}.VIP), unsafe.Sizeof(Drama{}.Source), unsafe.Sizeof(Drama{}.Cover),
		unsafe.Sizeof(Drama{}.Episodes), unsafe.Sizeof(Drama{}.SortMetadata))
	t.Logf("剧库目录: %s", filepath.Dir(path))

	// 磁盘缓存方案：只把精简索引留在内存
	dir := t.TempDir()
	runtime.GC()
	var storeBefore, storeAfter runtime.MemStats
	runtime.ReadMemStats(&storeBefore)
	store := &libraryStore{dir: dir, byID: map[string]int{}, shards: map[uint16]*libraryShard{}, dirty: map[uint16]bool{}}
	store.importDramas(cache.Dramas)
	if err := store.Persist(dir, &libraryIndexFile{}); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	// 释放完整记录，只保留索引（模拟稳态）
	store.shards = map[uint16]*libraryShard{}
	store.shardLRU = nil
	runtime.GC()
	runtime.ReadMemStats(&storeAfter)
	runtime.KeepAlive(store)
	indexHeap := storeAfter.HeapAlloc - storeBefore.HeapAlloc
	t.Logf("磁盘缓存索引: %.1f MiB（%d 部）", float64(indexHeap)/(1<<20), store.Len())
	if heapUsed > 0 {
		t.Logf("索引占全量内存的比例: %.0f%%（节省 %.1f MiB）",
			float64(indexHeap)/float64(heapUsed)*100, float64(heapUsed-indexHeap)/(1<<20))
	}
	t.Logf("dramaEntry 结构体静态大小: %d 字节", unsafe.Sizeof(dramaEntry{}))
	// 索引各字段的总字节数，定位剩余大头
	var sizes = []struct {
		name string
		size int
	}{{"Title", 0}, {"Name", 0}, {"Desc", 0}, {"Intro", 0}, {"Cover", 0}, {"其他", 0}}
	{
		entries := store.Entries()
		other := 0
		for i := range entries {
			e := &entries[i]
			sizes[0].size += len(e.Title)
			sizes[1].size += len(e.Name)
			sizes[2].size += len(e.Desc)
			sizes[3].size += len(e.Intro)
			sizes[4].size += len(coverString(e.Cover))
			other += len(e.Source) + len(e.SourceID) + len(e.ChannelName) + len(e.Category) + len(e.CategoryName) +
				len(e.TypeName) + len(e.SortName) + len(e.Remark) + len(e.Score) + len(e.Views) + len(e.Heat) +
				len(e.OnlineDate) + len(e.ReleaseStatus)
			for _, tag := range e.Tags {
				other += len(tag)
			}
		}
		sizes[5].size = other
		for _, item := range sizes {
			t.Logf("  索引字段 %s: %.1f MiB", item.name, float64(item.size)/(1<<20))
		}
	}

	// 冷启动：重新打开只读索引
	runtime.GC()
	var openBefore, openAfter runtime.MemStats
	runtime.ReadMemStats(&openBefore)
	reopened, err := openLibraryStore(dir)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&openAfter)
	runtime.KeepAlive(reopened)
	t.Logf("冷启动只读索引: %.1f MiB（%d 部）", float64(openAfter.HeapAlloc-openBefore.HeapAlloc)/(1<<20), reopened.Len())
}
