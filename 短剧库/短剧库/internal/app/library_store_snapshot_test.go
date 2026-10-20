package app

import "testing"

func snapshotFixtureStore(t *testing.T) *libraryStore {
	t.Helper()
	return &libraryStore{
		dir:    t.TempDir(),
		byID:   map[string]int{},
		shards: map[uint16]*libraryShard{},
		dirty:  map[uint16]bool{},
	}
}

// 列表请求共享同一份条目快照；库变更后必须换新快照，旧快照保持原样（编码在锁外进行）。
func TestLibraryStoreEntriesShareSnapshotUntilMutation(t *testing.T) {
	store := snapshotFixtureStore(t)
	store.Upsert([]Drama{{ID: "a", Name: "甲"}, {ID: "b", Name: "乙"}})
	first := store.Entries()
	second := store.Entries()
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("条目数量异常：%d / %d", len(first), len(second))
	}
	if &first[0] != &second[0] {
		t.Fatal("未变更时不该重新复制快照")
	}
	store.Upsert([]Drama{{ID: "c", Name: "丙"}})
	third := store.Entries()
	if len(third) != 3 {
		t.Fatalf("变更后快照应包含 3 条，实际 %d", len(third))
	}
	if &third[0] == &first[0] {
		t.Fatal("变更后必须换新快照，否则并发编码会读到撕裂数据")
	}
	if len(first) != 2 || first[0].ID != "a" {
		t.Fatalf("旧快照被就地修改：%+v", first)
	}
}

// 空闲回收后快照被丢弃，下一次读取重新发布。
func TestLibraryStoreDropsIdlePublishedSnapshot(t *testing.T) {
	store := snapshotFixtureStore(t)
	store.Upsert([]Drama{{ID: "a", Name: "甲"}})
	before := store.Entries()
	if len(before) != 1 {
		t.Fatalf("快照异常：%d", len(before))
	}
	if store.dropIdlePublished(24 * 60 * 60 * 1e9) {
		t.Fatal("快照还很新，不该丢弃")
	}
	if !store.dropIdlePublished(0) {
		t.Fatal("空闲超时后应丢弃快照")
	}
	after := store.Entries()
	if len(after) != 1 || &after[0] == &before[0] {
		t.Fatal("丢弃后应重建一份新快照")
	}
}
