package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 构造 n 部剧，ID 分散在不同分片上。
func libraryStoreFixtureDramas(count int) []Drama {
	dramas := make([]Drama, 0, count)
	for index := 0; index < count; index++ {
		dramas = append(dramas, Drama{
			ID:           fmt.Sprintf("hongguo:%d", 7000000000000000000+index),
			Source:       sourceHongguo,
			Title:        fmt.Sprintf("剧集%d", index),
			Desc:         fmt.Sprintf("概述%d", index),
			Intro:        fmt.Sprintf("简介%d，用于验证磁盘字段", index),
			Cover:        fmt.Sprintf("https://cover.example/%d.jpg", index),
			CoverURL:     fmt.Sprintf("https://cover.example/%d.jpg", index),
			TotalEpisode: fmt.Sprintf("%d", 20+index%60),
			Episodes:     []any{map[string]any{"title": "第1集"}},
			Duration:     "135",
		})
	}
	return dramas
}

func TestLibraryStorePersistAndReopenKeepsDiskFields(t *testing.T) {
	dir := t.TempDir()
	store := newLibraryStoreWith([]Drama{
		{ID: "hongguo:1", Source: sourceHongguo, Title: "甲", Desc: "概述甲", Intro: "简介甲", Cover: "https://cover/1.jpg", CoverURL: "https://cover/1.jpg", TotalEpisode: "30", Episodes: []any{"第1集"}, Duration: "120"},
		{ID: "hongguo:2", Source: sourceHongguo, Title: "乙", Desc: "概述乙", Intro: "简介乙", TotalEpisode: "45"},
	})
	if err := store.Persist(dir, &libraryIndexFile{}); err != nil {
		t.Fatal(err)
	}
	reopened, err := openLibraryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != 2 {
		t.Fatalf("重新打开的剧库数量 = %d, want 2", reopened.Len())
	}
	drama, found := reopened.Get("hongguo:1")
	if !found {
		t.Fatal("重新打开后丢失剧集")
	}
	// 索引字段
	if drama.Title != "甲" || drama.Desc != "概述甲" {
		t.Fatalf("索引字段丢失: %+v", drama)
	}
	// 仅存磁盘的重字段
	if drama.Intro != "简介甲" || fmt.Sprint(drama.TotalEpisode) != "30" || drama.Duration != "120" {
		t.Fatalf("磁盘字段丢失: %+v", drama)
	}
	if coverString(drama.Cover) != "https://cover/1.jpg" || coverString(drama.CoverURL) != "https://cover/1.jpg" {
		t.Fatalf("封面字段丢失: %+v", drama)
	}
}

func TestLibraryStoreScanUsesIndexWithoutDiskRecords(t *testing.T) {
	store := newLibraryStoreWith([]Drama{
		{ID: "hongguo:1", Source: sourceHongguo, Title: "甲", Desc: "概述甲", Intro: "简介甲", Cover: "https://cover/1.jpg", Episodes: []any{"第1集"}, Duration: "120"},
	})
	// 清空分片缓存并指向不存在的目录：模拟冷启动后只剩索引的状态
	store.dir = filepath.Join(t.TempDir(), "missing")
	store.shardMu.Lock()
	store.shards = map[uint16]*libraryShard{}
	store.shardLRU = nil
	store.dirty = map[uint16]bool{}
	store.shardMu.Unlock()

	drama, found := store.Get("hongguo:1")
	if !found {
		t.Fatal("索引命中失败")
	}
	// 列表、搜索、卡片、封面代理等热路径需要的字段必须仍然可用（含简介，前端本地搜索要用）
	if drama.Title != "甲" || drama.Desc != "概述甲" || drama.Intro != "简介甲" || coverString(drama.Cover) != "https://cover/1.jpg" {
		t.Fatalf("热路径字段不应依赖磁盘记录: %+v", drama)
	}
	// 分集列表等重字段此时为空，不影响扫描类功能
	if drama.Episodes != nil || drama.Duration != nil || drama.TotalEpisode != nil {
		t.Fatalf("分集/时长属于磁盘字段，索引态应为空: %+v", drama)
	}
}

func TestLibraryStorePersistOnlyRewritesTouchedShards(t *testing.T) {
	dir := t.TempDir()
	dramas := libraryStoreFixtureDramas(40)
	store := newLibraryStoreWith(dramas)
	if err := store.Persist(dir, &libraryIndexFile{}); err != nil {
		t.Fatal(err)
	}
	snapshot := map[uint16]string{}
	for _, drama := range dramas {
		shard := dramaShard(drama.ID)
		body, err := os.ReadFile(libraryShardPath(dir, shard))
		if err != nil {
			t.Fatal(err)
		}
		snapshot[shard] = string(body)
	}
	target := dramas[0].ID
	if !store.Update(target, func(drama *Drama) bool {
		drama.Title = "改过的标题"
		return true
	}) {
		t.Fatal("更新剧集失败")
	}
	if err := store.Persist(dir, &libraryIndexFile{}); err != nil {
		t.Fatal(err)
	}
	changed := 0
	targetShard := dramaShard(target)
	for shard, before := range snapshot {
		body, err := os.ReadFile(libraryShardPath(dir, shard))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != before {
			changed++
			if shard != targetShard {
				t.Errorf("分片 %d 未改动却被重写", shard)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("重写分片数 = %d, want 1", changed)
	}
}

func TestLibraryStoreEvictsShardsAndReloads(t *testing.T) {
	dir := t.TempDir()
	dramas := libraryStoreFixtureDramas(200)
	store := newLibraryStoreWith(dramas)
	store.dir = dir
	if err := store.Persist(dir, &libraryIndexFile{}); err != nil {
		t.Fatal(err)
	}
	// 读回全部，触发分片加载与淘汰
	for _, drama := range dramas {
		loaded, found := store.Get(drama.ID)
		if !found {
			t.Fatalf("分片淘汰后丢失剧集 %s", drama.ID)
		}
		if loaded.Intro != drama.Intro {
			t.Fatalf("分片重新加载后磁盘字段不一致: %s", drama.ID)
		}
	}
	if _, cached := store.Stats(); cached > libraryShardCacheLimit*2 {
		t.Fatalf("常驻分片数 = %d，应受 LRU 限制", cached)
	}
}

func TestLibraryStoreUpsertKeepsExistingDiskFields(t *testing.T) {
	store := newLibraryStoreWith([]Drama{
		{ID: "hongguo:1", Source: sourceHongguo, Title: "旧标题", Intro: "旧简介", TotalEpisode: "60", Episodes: []any{"第1集"}},
	})
	store.Upsert([]Drama{{ID: "hongguo:1", Source: sourceHongguo, Title: "新标题"}})
	if store.Len() != 1 {
		t.Fatalf("Upsert 后数量 = %d, want 1", store.Len())
	}
	drama, _ := store.Get("hongguo:1")
	if drama.Title != "新标题" {
		t.Fatalf("标题未更新: %+v", drama)
	}
	if drama.Intro != "旧简介" || fmt.Sprint(drama.TotalEpisode) != "60" {
		t.Fatalf("合并时丢失了磁盘已有字段: %+v", drama)
	}
	// 索引条目也应反映合并结果
	entry, _ := store.Entry("hongguo:1")
	if entry.Title != "新标题" {
		t.Fatalf("索引条目未同步: %+v", entry)
	}
}

func TestLibraryStoreSortByTitleKeepsRecords(t *testing.T) {
	store := newLibraryStoreWith([]Drama{
		{ID: "hongguo:1", Source: sourceHongguo, Title: "c", Intro: "简介c"},
		{ID: "hongguo:2", Source: sourceHongguo, Title: "a", Intro: "简介a"},
		{ID: "hongguo:3", Source: sourceHongguo, Title: "b", Intro: "简介b"},
	})
	store.SortByTitle()
	all := store.All()
	if len(all) != 3 || all[0].Title != "a" || all[1].Title != "b" || all[2].Title != "c" {
		t.Fatalf("按标题排序失败: %s %s %s", all[0].Title, all[1].Title, all[2].Title)
	}
	if all[1].Intro != "简介b" {
		t.Fatalf("排序后磁盘字段错位: %+v", all[1])
	}
}

func TestLibraryStoreRemoveDropsIndexAndRecord(t *testing.T) {
	store := newLibraryStoreWith([]Drama{
		{ID: "hongguo:1", Source: sourceHongguo, Title: "甲", Intro: "简介甲"},
		{ID: "hongguo:2", Source: sourceHongguo, Title: "乙"},
	})
	store.Remove([]string{"hongguo:1"})
	if store.Len() != 1 {
		t.Fatalf("删除后数量 = %d, want 1", store.Len())
	}
	if _, found := store.Get("hongguo:1"); found {
		t.Fatal("记录未删除")
	}
	if _, found := store.Entry("hongguo:1"); found {
		t.Fatal("索引未删除")
	}
}

func TestLibraryStoreMigratesLegacyWholeFileCache(t *testing.T) {
	dir := t.TempDir()
	rows := libraryStoreFixtureDramas(12)
	if err := writeLibraryCache(dir, libraryCache{Dramas: rows}); err != nil {
		t.Fatal(err)
	}
	// 旧格式必须能被 readLibraryCache 读到（迁移前）
	legacy, err := readLibraryCache(dir)
	if err != nil || len(legacy.Dramas) != len(rows) {
		t.Fatalf("旧格式读取失败: %v", err)
	}
	// 写入分片剧库后，读取应以分片为准
	store := &libraryStore{dir: dir, byID: map[string]int{}, shards: map[uint16]*libraryShard{}, dirty: map[uint16]bool{}}
	store.importDramas(rows)
	if err := store.Persist(dir, &libraryIndexFile{}); err != nil {
		t.Fatal(err)
	}
	reopened, err := openLibraryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != len(rows) {
		t.Fatalf("分片剧库数量 = %d, want %d", reopened.Len(), len(rows))
	}
	merged, err := readLibraryCache(dir)
	if err != nil || len(merged.Dramas) != len(rows) {
		t.Fatalf("分片剧库读取失败: %v", err)
	}
	if merged.Dramas[0].Intro != rows[0].Intro {
		t.Fatalf("分片剧库字段不完整: %+v", merged.Dramas[0])
	}
}
