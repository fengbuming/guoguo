package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// libraryStore 把完整剧集记录放在磁盘（按 ID 哈希分 256 个分片），内存只保留精简索引。
// 扫描类操作（列表、搜索、统计、排序、追剧判定）只访问索引，零磁盘 IO；
// 需要完整记录时（详情、导出、合并、同步包）按分片读取，并由 LRU 缓存复用。

const libraryShardCacheLimit = 16

type libraryStore struct {
	dir string

	mu      sync.RWMutex
	entries []dramaEntry
	byID    map[string]int
	order   []int // entries 的下标顺序，保持与写入顺序一致
	// published 是给读路径共享的不可变条目快照；库变更后第一次读取时才复制一次。
	published      []dramaEntry
	publishedDirty bool
	publishedAt    time.Time

	shardMu   sync.Mutex
	shards    map[uint16]*libraryShard
	shardLRU  []uint16
	dirty     map[uint16]bool
	dirtyMeta bool

	meta    libraryIndexFile
	hasMeta bool
}

type libraryShard struct {
	records map[string]Drama
}

type libraryIndexFile struct {
	Version int           `json:"version"`
	Count   int           `json:"count"`
	Entries []dramaEntry  `json:"entries"`
	SavedAt time.Time     `json:"savedAt,omitempty"`
	Sources map[string]librarySourceState    `json:"sources,omitempty"`
	Hongguo *hongguoCatalogState             `json:"hongguoApp,omitempty"`
	Catalog map[string]providerCatalogCursor `json:"providerCatalog,omitempty"`
	LoadedAt time.Time   `json:"loadedAt,omitempty"`
	LastError string     `json:"lastError,omitempty"`
}

func libraryStoreDir(dataDir string) string {
	return filepath.Join(dataDir, "library")
}

func libraryIndexPath(dataDir string) string {
	return filepath.Join(libraryStoreDir(dataDir), "index.json")
}

func libraryShardPath(dataDir string, shard uint16) string {
	return filepath.Join(libraryStoreDir(dataDir), "records", fmt.Sprintf("%03d.json", shard))
}

// openLibraryStore 读取索引建立内存条目；记录文件按需加载。
func openLibraryStore(dataDir string) (*libraryStore, error) {
	store := &libraryStore{
		dir:    dataDir,
		byID:   map[string]int{},
		shards: map[uint16]*libraryShard{},
		dirty:  map[uint16]bool{},
	}
	body, err := os.ReadFile(libraryIndexPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var index libraryIndexFile
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, fmt.Errorf("剧库索引损坏 %s: %w", libraryIndexPath(dataDir), err)
	}
	store.meta, store.hasMeta = index, true
	store.entries = make([]dramaEntry, 0, len(index.Entries))
	for _, entry := range index.Entries {
		if entry.ID == "" {
			continue
		}
		if _, exists := store.byID[entry.ID]; exists {
			continue
		}
		entry.Shard = dramaShard(entry.ID)
		store.byID[entry.ID] = len(store.entries)
		store.entries = append(store.entries, entry)
	}
	return store, nil
}

// importDramas 用完整记录重建整库（迁移旧缓存、导入同步包、测试构造）。
func (s *libraryStore) importDramas(dramas []Drama) {
	entries := make([]dramaEntry, 0, len(dramas))
	byID := make(map[string]int, len(dramas))
	shards := make(map[uint16]map[string]Drama, libraryShardCount)
	for _, drama := range dramas {
		if drama.ID == "" {
			continue
		}
		if _, exists := byID[drama.ID]; exists {
			continue
		}
		entry := newDramaEntry(drama)
		byID[drama.ID] = len(entries)
		entries = append(entries, entry)
		shard := dramaShard(drama.ID)
		if shards[shard] == nil {
			shards[shard] = map[string]Drama{}
		}
		full := drama
		entryRef := entry
		entryRef.applyEntry(&full)
		shards[shard][drama.ID] = full
	}
	s.mu.Lock()
	s.entries, s.byID = entries, byID
	s.entriesChangedLocked()
	s.mu.Unlock()

	s.shardMu.Lock()
	s.shards = make(map[uint16]*libraryShard, len(shards))
	s.shardLRU = nil
	s.dirty = map[uint16]bool{}
	for shard, records := range shards {
		s.shards[shard] = &libraryShard{records: records}
		s.dirty[shard] = true
	}
	s.dirtyMeta = true
	s.shardMu.Unlock()
}

// appLibrary 保证 UIApp 已挂载剧库存储（懒初始化，避免空指针）。
func (a *UIApp) appLibrary() *libraryStore {
	if a.library == nil {
		a.library = &libraryStore{dir: a.cfg.dataDirectory(), byID: map[string]int{}, shards: map[uint16]*libraryShard{}, dirty: map[uint16]bool{}}
	}
	return a.library
}

// libraryLen 返回剧集数量。调用方需持有 a.mu。
func (a *UIApp) libraryLen() int {
	return a.appLibrary().Len()
}

// libraryEntries 返回只读索引切片，供扫描类逻辑使用（零磁盘 IO）。调用方需持有 a.mu。
func (a *UIApp) libraryEntries() []dramaEntry {
	return a.appLibrary().Entries()
}

// libraryIndexDramas 用索引条目构造 Drama 列表（不含简介等磁盘字段），
// 供只需要 ID/站源/排序元数据的批量任务使用，避免全量读取记录文件。
func (a *UIApp) libraryIndexDramas() []Drama {
	entries := a.libraryEntries()
	result := make([]Drama, 0, len(entries))
	for i := range entries {
		result = append(result, entries[i].drama(nil))
	}
	return result
}

// libraryDrama 读取单部剧的完整记录（含简介、封面别名等磁盘字段）。
func (a *UIApp) libraryDrama(id string) (Drama, bool) {
	return a.appLibrary().Get(id)
}

// newLibraryStoreWith 构造一个内存态剧库存储，供测试与一次性导入使用。
func newLibraryStoreWith(dramas []Drama) *libraryStore {
	store := &libraryStore{byID: map[string]int{}, shards: map[uint16]*libraryShard{}, dirty: map[uint16]bool{}}
	store.importDramas(dramas)
	return store
}

// mutateDramaAt 按下标读取剧集、就地修改并写回，供测试与维护脚本使用。
func (a *UIApp) mutateDramaAt(index int, fn func(*Drama)) {
	list := a.libraryDramas()
	if index < 0 || index >= len(list) || fn == nil {
		return
	}
	fn(&list[index])
	a.setLibraryDramas(list)
}

// libraryDramas 读取全部完整记录，仅用于同步包、Emby 导出与测试等场景。
func (a *UIApp) libraryDramas() []Drama {
	return a.appLibrary().All()
}

// setLibraryDramas 用完整记录重建剧库（导入同步包、刷新结果、测试构造）。
func (a *UIApp) setLibraryDramas(dramas []Drama) {
	a.appLibrary().importDramas(dramas)
}

// libraryUpsert 合并若干完整记录进剧库，仅涉及的分片会标记落盘。
func (a *UIApp) libraryUpsert(dramas []Drama) {
	a.appLibrary().Upsert(dramas)
}

// libraryUpdate 读取单部剧完整记录并原地修改，返回是否发生变更。
func (a *UIApp) libraryUpdate(id string, fn func(*Drama) bool) bool {
	return a.appLibrary().Update(id, fn)
}

// libraryUpdateEntries 只改动索引条目，不读取记录文件。
func (a *UIApp) libraryUpdateEntries(ids []string, fn func(*dramaEntry) bool) {
	a.appLibrary().UpdateEntries(ids, fn)
}

// libraryMerge 合并若干完整记录并保持按标题排序，等价于旧的整库 mergeSourceDramas，
// 但只读写涉及的分片，不重建整库。
func (a *UIApp) libraryMerge(dramas []Drama) {
	if len(dramas) == 0 {
		return
	}
	a.appLibrary().Upsert(dramas)
	a.appLibrary().SortByTitle()
}

// libraryMergeKeep 合并但以库中已有记录为准（过期榜单回写时使用）。
func (a *UIApp) libraryMergeKeep(dramas []Drama) {
	if len(dramas) == 0 {
		return
	}
	a.appLibrary().UpsertKeep(dramas)
	a.appLibrary().SortByTitle()
}

// Len 返回剧集数量。
func (s *libraryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Entries 返回只读索引切片，调用方持有的锁需覆盖使用期（与既有 a.mu 语义一致）。
// Entries 返回共享的只读条目快照。
//
// 快照写时复制：库变更后第一次读取才复制一次，之后所有请求共用同一份。
// 调用方只能读，不能就地排序或修改（要改自己复制）。
func (s *libraryStore) Entries() []dramaEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publishedDirty || s.published == nil && len(s.entries) > 0 {
		published := make([]dramaEntry, len(s.entries))
		copy(published, s.entries)
		s.published, s.publishedDirty, s.publishedAt = published, false, time.Now()
	}
	return s.published
}

// dropIdlePublished 丢掉闲置的共享快照，让空闲期常驻内存回到只含索引的水平。
// 下一次列表请求会重建一次（10MB 级），并发突发仍然共享同一份。
func (s *libraryStore) dropIdlePublished(idle time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.published == nil || time.Since(s.publishedAt) < idle {
		return false
	}
	s.published, s.publishedDirty = nil, true
	return true
}

// entriesChangedLocked 标记共享快照失效；调用方必须持有 s.mu 写锁。
func (s *libraryStore) entriesChangedLocked() {
	s.publishedDirty = true
}

// Entry 返回单条索引，found 为 false 表示不存在。
func (s *libraryStore) Entry(id string) (dramaEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index, found := s.byID[id]
	if !found {
		return dramaEntry{}, false
	}
	return s.entries[index], true
}

// Get 读取完整记录；命中内存分片缓存时不产生磁盘 IO。
func (s *libraryStore) Get(id string) (Drama, bool) {
	entry, found := s.Entry(id)
	if !found {
		return Drama{}, false
	}
	full, ok := s.shardRecord(entry.Shard, id)
	if !ok {
		return entry.drama(nil), true
	}
	return entry.drama(&full), true
}

// All 读取全部完整记录，仅用于同步包导出、Emby 导出、整库合并与测试。
// 按分片分组顺序读取，保证每个分片文件最多读一次，避免缓存抖动。
func (s *libraryStore) All() []Drama {
	s.mu.RLock()
	entries := make([]dramaEntry, len(s.entries))
	copy(entries, s.entries)
	s.mu.RUnlock()
	byShard := make(map[uint16][]string, libraryShardCount)
	for i := range entries {
		byShard[entries[i].Shard] = append(byShard[entries[i].Shard], entries[i].ID)
	}
	found := make(map[string]Drama, len(entries))
	shards := make([]uint16, 0, len(byShard))
	for shard := range byShard {
		shards = append(shards, shard)
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i] < shards[j] })
	for _, shard := range shards {
		s.shardMu.Lock()
		records := s.loadShardLocked(shard)
		s.shardMu.Unlock()
		for _, id := range byShard[shard] {
			if drama, ok := records[id]; ok {
				found[id] = drama
			}
		}
	}
	result := make([]Drama, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		if full, ok := found[entry.ID]; ok {
			result = append(result, entry.drama(&full))
			continue
		}
		result = append(result, entry.drama(nil))
	}
	return result
}

// Upsert 写入或更新记录；已存在时以 incoming 为准重建索引条目与磁盘记录。
func (s *libraryStore) Upsert(dramas []Drama) {
	s.upsert(dramas, false)
}

// UpsertKeep 写入或更新记录，但已存在时保留库中记录为准（用于过期榜单等场景）。
func (s *libraryStore) UpsertKeep(dramas []Drama) {
	s.upsert(dramas, true)
}

func (s *libraryStore) upsert(dramas []Drama, existingWins bool) {
	if len(dramas) == 0 {
		return
	}
	byShard := make(map[uint16][]Drama, len(dramas))
	for _, drama := range dramas {
		if drama.ID == "" {
			continue
		}
		shard := dramaShard(drama.ID)
		byShard[shard] = append(byShard[shard], drama)
	}
	// 先读分片并算出合并后的完整记录，再用它重建索引，保证索引与磁盘记录一致。
	merged := make(map[string]Drama, len(dramas))
	s.shardMu.Lock()
	for shard, items := range byShard {
		records := s.loadShardLocked(shard)
		for _, drama := range items {
			full := drama
			if previous, found := records[drama.ID]; found {
				if existingWins {
					full = mergeDramaMetadata(previous, drama)
				} else {
					full = mergeDramaMetadata(drama, previous)
				}
			}
			refreshed := newDramaEntry(full)
			refreshed.applyEntry(&full)
			records[drama.ID] = full
			merged[drama.ID] = full
		}
		s.dirty[shard] = true
	}
	s.dirtyMeta = true
	s.shardMu.Unlock()

	s.mu.Lock()
	for _, drama := range dramas {
		if drama.ID == "" {
			continue
		}
		full, found := merged[drama.ID]
		if !found {
			continue
		}
		entry := newDramaEntry(full)
		if position, exists := s.byID[drama.ID]; exists {
			s.entries[position] = entry
		} else {
			s.byID[drama.ID] = len(s.entries)
			s.entries = append(s.entries, entry)
		}
	}
	s.entriesChangedLocked()
	s.mu.Unlock()
}

// Update 读取完整记录并原地修改，fn 返回 true 表示已修改需要落盘。
func (s *libraryStore) Update(id string, fn func(*Drama) bool) bool {
	entry, found := s.Entry(id)
	if !found {
		return false
	}
	s.shardMu.Lock()
	records := s.loadShardLocked(entry.Shard)
	full, ok := records[id]
	if !ok {
		full = entry.drama(nil)
	}
	entry.applyEntry(&full)
	if !fn(&full) {
		s.shardMu.Unlock()
		return false
	}
	records[id] = full
	s.dirty[entry.Shard] = true
	s.dirtyMeta = true
	s.shardMu.Unlock()
	s.mu.Lock()
	if position, exists := s.byID[id]; exists {
		s.entries[position] = newDramaEntry(full)
	}
	s.entriesChangedLocked()
	s.mu.Unlock()
	return true
}

// SortByTitle 按展示标题重排索引，等价于 mergeSourceDramas 结果里的全局排序，
// 但不触碰记录文件，因此可以在合并后廉价地保持原有顺序语义。
func (s *libraryStore) SortByTitle() {
	s.mu.Lock()
	sort.SliceStable(s.entries, func(i, j int) bool {
		return entryDisplayTitle(s.entries[i]) < entryDisplayTitle(s.entries[j])
	})
	for index := range s.entries {
		s.byID[s.entries[index].ID] = index
	}
	s.entriesChangedLocked()
	s.mu.Unlock()
	s.shardMu.Lock()
	s.dirtyMeta = true
	s.shardMu.Unlock()
}

// UpdateEntries 只更新索引条目并标记对应分片待落盘，适合排序元数据这类仅索引字段的变更。
func (s *libraryStore) UpdateEntries(ids []string, fn func(*dramaEntry) bool) {
	if len(ids) == 0 || fn == nil {
		return
	}
	s.mu.Lock()
	shards := map[uint16]bool{}
	for _, id := range ids {
		position, found := s.byID[id]
		if !found {
			continue
		}
		if !fn(&s.entries[position]) {
			continue
		}
		shards[s.entries[position].Shard] = true
	}
	s.entriesChangedLocked()
	s.mu.Unlock()
	if len(shards) == 0 {
		return
	}
	s.shardMu.Lock()
	for shard := range shards {
		s.dirty[shard] = true
	}
	s.dirtyMeta = true
	s.shardMu.Unlock()
}

// Remove 删除记录。
func (s *libraryStore) Remove(ids []string) {
	if len(ids) == 0 {
		return
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	shards := map[uint16]bool{}
	s.mu.Lock()
	remaining := make([]dramaEntry, 0, len(s.entries))
	byID := make(map[string]int, len(s.entries))
	for _, entry := range s.entries {
		if drop[entry.ID] {
			shards[entry.Shard] = true
			continue
		}
		byID[entry.ID] = len(remaining)
		remaining = append(remaining, entry)
	}
	s.entries, s.byID = remaining, byID
	s.entriesChangedLocked()
	s.mu.Unlock()

	s.shardMu.Lock()
	for shard := range shards {
		records := s.loadShardLocked(shard)
		for id := range drop {
			if _, found := records[id]; found {
				delete(records, id)
				s.dirty[shard] = true
			}
		}
	}
	s.dirtyMeta = true
	s.shardMu.Unlock()
}

// Persist 落盘：先写改动过的分片，再写索引，均为临时文件 + 原子改名。
func (s *libraryStore) Persist(dataDir string, meta *libraryIndexFile) error {
	s.shardMu.Lock()
	type pending struct {
		shard   uint16
		records map[string]Drama
	}
	var queue []pending
	for shard := range s.dirty {
		if records := s.shardRecordsLocked(shard); records != nil {
			queue = append(queue, pending{shard: shard, records: records})
		}
	}
	s.dirty = map[uint16]bool{}
	writeMeta := s.dirtyMeta
	s.dirtyMeta = false
	s.shardMu.Unlock()

	if err := os.MkdirAll(filepath.Join(libraryStoreDir(dataDir), "records"), 0o755); err != nil {
		return err
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].shard < queue[j].shard })
	for _, item := range queue {
		// 落盘前用索引条目覆盖记录中的同名字段，保证磁盘记录与内存一致
		s.applyEntriesToRecords(item.records)
		if err := writeShardFile(libraryShardPath(dataDir, item.shard), item.records); err != nil {
			return err
		}
	}
	if !writeMeta || meta == nil {
		return nil
	}
	s.mu.RLock()
	entries := make([]dramaEntry, len(s.entries))
	copy(entries, s.entries)
	s.mu.RUnlock()
	meta.Version = libraryStoreVersion
	meta.Count = len(entries)
	meta.Entries = entries
	meta.SavedAt = time.Now()
	return writeJSONFile(libraryIndexPath(dataDir), meta)
}

// applyEntriesToRecords 把索引条目的字段写回待落盘的分片记录。
func (s *libraryStore) applyEntriesToRecords(records map[string]Drama) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, drama := range records {
		position, found := s.byID[id]
		if !found {
			continue
		}
		entry := s.entries[position]
		entry.applyEntry(&drama)
		records[id] = drama
	}
}

// Stats 暴露存储状态，便于测试与排查。
func (s *libraryStore) Stats() (entries int, shards int) {
	s.mu.RLock()
	entries = len(s.entries)
	s.mu.RUnlock()
	s.shardMu.Lock()
	shards = len(s.shards)
	s.shardMu.Unlock()
	return entries, shards
}

func (s *libraryStore) shardRecord(shard uint16, id string) (Drama, bool) {
	s.shardMu.Lock()
	defer s.shardMu.Unlock()
	records := s.loadShardLocked(shard)
	drama, found := records[id]
	return drama, found
}

func (s *libraryStore) shardRecordsLocked(shard uint16) map[string]Drama {
	data := s.loadShardLocked(shard)
	if data == nil {
		return nil
	}
	return data
}

// loadShardLocked 读取分片并维护 LRU；调用方需持有 shardMu。
func (s *libraryStore) loadShardLocked(shard uint16) map[string]Drama {
	if data, found := s.shards[shard]; found {
		s.touchShardLocked(shard)
		s.evictShardsLocked()
		return data.records
	}
	records := map[string]Drama{}
	body, err := os.ReadFile(libraryShardPath(s.dir, shard))
	if err == nil && len(body) > 0 {
		var file struct {
			Dramas []Drama `json:"dramas"`
		}
		if err := json.Unmarshal(body, &file); err == nil {
			for _, drama := range file.Dramas {
				if drama.ID != "" {
					records[drama.ID] = drama
				}
			}
		}
	}
	s.shards[shard] = &libraryShard{records: records}
	s.touchShardLocked(shard)
	s.evictShardsLocked()
	return records
}

func (s *libraryStore) touchShardLocked(shard uint16) {
	for index, value := range s.shardLRU {
		if value == shard {
			s.shardLRU = append(s.shardLRU[:index], s.shardLRU[index+1:]...)
			break
		}
	}
	s.shardLRU = append(s.shardLRU, shard)
}

// evictShardsLocked 把最久未用的分片移出内存。脏分片先就地落盘再淘汰，
// 这样即使长时间不调 Persist，内存里的分片数量也不会无限增长。
// 若分片无法落盘（如纯内存态的测试存储），则保留在队列中并跳过，靠 attempts 保证不会死循环。
func (s *libraryStore) evictShardsLocked() {
	attempts := 0
	for len(s.shardLRU) > libraryShardCacheLimit && attempts <= len(s.shardLRU) {
		oldest := s.shardLRU[0]
		if s.dirty[oldest] && !s.flushShardLocked(oldest) {
			s.shardLRU = append(s.shardLRU[1:], oldest)
			attempts++
			continue
		}
		s.shardLRU = s.shardLRU[1:]
		delete(s.shards, oldest)
		attempts = 0
	}
}

// flushShardLocked 把单个脏分片写到磁盘并清除脏标记，调用方需持有 shardMu。
func (s *libraryStore) flushShardLocked(shard uint16) bool {
	if s.dir == "" {
		return false
	}
	data, found := s.shards[shard]
	if !found || data == nil {
		return true
	}
	records := make(map[string]Drama, len(data.records))
	for id, drama := range data.records {
		records[id] = drama
	}
	s.applyEntriesToRecords(records)
	if err := writeShardFile(libraryShardPath(s.dir, shard), records); err != nil {
		return false
	}
	delete(s.dirty, shard)
	return true
}

func writeShardFile(path string, records map[string]Drama) error {
	items := make([]Drama, 0, len(records))
	for _, drama := range records {
		items = append(items, drama)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return writeJSONFile(path, map[string]any{"dramas": items})
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".library-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	body, err := json.Marshal(value)
	if err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
