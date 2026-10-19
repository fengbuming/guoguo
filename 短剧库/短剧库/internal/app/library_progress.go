package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type librarySourceState struct {
	Status    string    `json:"status"`
	Count     int       `json:"count"`
	Error     string    `json:"error,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (state librarySourceState) withPhase(phase string) librarySourceState {
	phase = strings.TrimSpace(phase)
	if state.Status == "loading" {
		state.Phase = phase
	}
	return state
}

type libraryProgressKey struct{}
type libraryCategoryKey struct{}
type libraryProgressFunc func(string, string, []Drama, error, bool) error

type libraryLoadMode int

const (
	libraryLoadRefresh libraryLoadMode = iota
	libraryLoadMore
	libraryLoadUpdate
)

func reportLibraryProgress(ctx context.Context, source string, items []Drama, err error, done bool) error {
	if callback, ok := ctx.Value(libraryProgressKey{}).(libraryProgressFunc); ok && (done || len(items) > 0) {
		category, _ := ctx.Value(libraryCategoryKey{}).(string)
		return callback(source, category, items, err, done)
	}
	return nil
}

func (a *UIApp) setLibraryPhaseLocked(phase string) {
	phase = strings.TrimSpace(phase)
	a.libraryLoadingPhase = phase
	if a.librarySources == nil {
		a.librarySources = map[string]librarySourceState{}
	}
	for source, state := range a.librarySources {
		if state.Status == "loading" {
			a.librarySources[source] = state.withPhase(phase)
		}
	}
}

func (a *UIApp) handleLibraryCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	a.mu.Lock()
	cancel := a.libraryCancel
	running := a.libraryLoading != nil
	if running {
		a.setLibraryPhaseLocked("正在停止，已保留已入库内容")
		a.libraryRevision++
	}
	resp, entries := a.librarySnapshotForSourceLocked(r.Context(), 0)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	resp["canceled"] = running
	writeLibrarySnapshot(w, http.StatusOK, resp, entries)
}

func (a *UIApp) startLibraryRefreshLocked(source string) {
	a.startLibraryLoadLocked(source, libraryLoadRefresh, nil, "")
}

func (a *UIApp) startLibraryLoadLocked(source string, mode libraryLoadMode, priority []string, category string, scopes ...context.Context) {
	category = strings.TrimSpace(category)
	base := context.Background()
	if len(scopes) > 0 && sourceScope(scopes[0]) != nil {
		base = context.WithValue(base, accountSourceContextKey{}, sourceScope(scopes[0]))
	}
	if mode == libraryLoadUpdate || mode == libraryLoadMore {
		if source == "" && sourceScopeRestricted(base) {
			for _, allowed := range sourceScope(base).Sources {
				a.enqueueHistoricalMetadataLocked(allowed, priority)
			}
		} else {
			a.enqueueHistoricalMetadataLocked(source, priority)
		}
	}
	more := mode == libraryLoadMore
	if more {
		hasMore := matchesSourceFilter(sourceHongguo, source) && sourceAllowed(base, sourceHongguo) && hongguoCatalogCategoryHasMore(a.libraryApp, category)
		for provider, cursor := range a.libraryProviders {
			if providerCatalogSource(provider) == provider {
				hasMore = hasMore || matchesSourceFilter(provider, source) && sourceAllowed(base, provider) && cursor.Initialized && !cursor.Exhausted
			}
		}
		if !hasMore {
			return
		}
	}
	previousCount := a.libraryLen()
	a.libraryAttempted = true
	a.libraryLoading = make(chan struct{})
	a.libraryMore = more
	a.libraryLoadingSource = source
	a.libraryLoadingPhase = "等待开始"
	a.libraryLoadingCategory = category
	if a.librarySources == nil {
		a.librarySources = map[string]librarySourceState{}
	}
	for _, provider := range []string{"cloudfront", sourceHuangguoAI, sourceHuangguoVideo, sourceHuangdou, sourceHongguo, sourceHuangju, sourceYeguo, sourceDSD} {
		if matchesSourceFilter(provider, source) && sourceAllowed(base, provider) {
			state := a.librarySources[provider]
			state.Status = "loading"
			state.Error = ""
			state.Phase = "等待开始"
			a.librarySources[provider] = state
		}
	}
	a.libraryRevision++
	ctx, cancel := context.WithTimeout(base, 12*time.Minute)
	a.libraryCancel = cancel
	ctx = context.WithValue(ctx, libraryProgressKey{}, libraryProgressFunc(a.acceptLibraryProgress))
	ctx = context.WithValue(ctx, libraryMoreKey{}, more)
	ctx = context.WithValue(ctx, libraryUpdateKey{}, mode == libraryLoadUpdate)
	if category != "" {
		ctx = context.WithValue(ctx, libraryCategoryKey{}, category)
	}
	ctx = withKnownHongguoDramas(ctx, a.libraryIndexDramas())
	go func() {
		defer cancel()
		var loadErr error
		a.mu.Lock()
		a.setLibraryPhaseLocked("保存当前剧库")
		a.libraryRevision++
		a.mu.Unlock()
		if err := a.persistLibraryIfDirty(); err != nil {
			loadErr = err
		} else if more {
			a.mu.Lock()
			a.setLibraryPhaseLocked("继续加载目录")
			a.libraryRevision++
			a.mu.Unlock()
			_, loadErr = a.downloader.fetchMoreLibrary(ctx, source)
		} else {
			a.mu.Lock()
			a.setLibraryPhaseLocked("读取站源目录")
			a.libraryRevision++
			a.mu.Unlock()
			_, loadErr = a.downloader.fetchAllDramas(ctx, source)
		}
		a.mu.Lock()
		if ctx.Err() == context.Canceled {
			loadErr = nil
			a.markLibrarySourcesCanceledLocked(base, source)
		} else if loadErr != nil && a.libraryLoading != nil {
			a.markLibrarySourcesFailedLocked(base, source, loadErr)
		}
		a.libraryDirty = true
		a.libraryRevision++
		a.mu.Unlock()
		_ = a.persistLibrary()
		a.mu.Lock()
		close(a.libraryLoading)
		a.libraryLoading = nil
		a.libraryCancel = nil
		a.libraryLoadingPhase = ""
		a.libraryLoadingCategory = ""
		a.libraryRevision++
		count := a.libraryLen()
		a.mu.Unlock()
		fmt.Printf("本地剧库：%d 部，本次新增 %d 部\n", count, count-previousCount)
	}()
}

func (a *UIApp) persistLibraryIfDirty() error {
	a.mu.Lock()
	dirty := a.libraryDirty
	a.mu.Unlock()
	if !dirty {
		return nil
	}
	return a.persistLibrary()
}

func (a *UIApp) markLibrarySourcesFailedLocked(ctx context.Context, source string, err error) {
	if a.librarySources == nil {
		a.librarySources = map[string]librarySourceState{}
	}
	for _, provider := range []string{"cloudfront", sourceHuangguoAI, sourceHuangguoVideo, sourceHuangdou, sourceHongguo, sourceHuangju, sourceYeguo, sourceDSD} {
		if !matchesSourceFilter(provider, source) || !sourceAllowed(ctx, provider) {
			continue
		}
		state := a.librarySources[provider]
		if state.Status == "loading" {
			state.Status = "failed"
			if state.Count > 0 {
				state.Status = "partial"
			}
			state.Error = a.redactError(err)
			state.Phase = ""
			a.librarySources[provider] = state
		}
	}
	a.libraryError = librarySourceErrors(a.librarySources)
}

func (a *UIApp) markLibrarySourcesCanceledLocked(ctx context.Context, source string) {
	if a.librarySources == nil {
		a.librarySources = map[string]librarySourceState{}
	}
	for _, provider := range []string{"cloudfront", sourceHuangguoAI, sourceHuangguoVideo, sourceHuangdou, sourceHongguo, sourceHuangju, sourceYeguo, sourceDSD} {
		if !matchesSourceFilter(provider, source) || !sourceAllowed(ctx, provider) {
			continue
		}
		state := a.librarySources[provider]
		if state.Status == "loading" {
			state.Status = "canceled"
			state.Error = ""
			state.Phase = ""
			a.librarySources[provider] = state
		}
	}
	a.libraryError = librarySourceErrors(a.librarySources)
}

func (a *UIApp) acceptLibraryProgress(source, category string, items []Drama, loadErr error, done bool) error {
	a.mu.Lock()
	items = append([]Drama(nil), items...)
	a.normalizeDramaCovers(items)
	newDramas := a.newSortMetadataDramasLocked(items)
	if source == sourceHongguo {
		a.libraryApp = a.downloader.hongguoCatalogSnapshot()
	}
	if paginatedProvider(source) {
		if a.libraryProviders == nil {
			a.libraryProviders = map[string]providerCatalogCursor{}
		}
		key := providerCatalogKey(source, category)
		if cursor, exists := a.downloader.providerCatalogSnapshot()[key]; exists {
			a.libraryProviders[key] = cursor
		}
	}
	if done {
		// 只合并本次命中的条目并保持标题排序，未涉及的剧集不产生磁盘读写
		a.libraryMerge(items)
	} else {
		// 增量合并只处理本次命中的条目，未涉及的剧集不产生磁盘读取。
		a.libraryUpsert(items)
	}
	a.enqueueSortMetadataLocked(newDramas, false)
	state := a.librarySources[source]
	state.Count = 0
	for _, entry := range a.libraryEntries() {
		if entryProvider(entry) == source {
			state.Count++
		}
	}
	if len(items) > 0 {
		a.loadedAt = time.Now()
		state.UpdatedAt = a.loadedAt
	}
	if done {
		state.Status = "ready"
		if loadErr != nil {
			state.Status = "failed"
			if state.Count > 0 {
				state.Status = "partial"
			}
			state.Error = a.redactError(loadErr)
		}
		state.Phase = ""
	}
	a.librarySources[source] = state
	a.libraryError = librarySourceErrors(a.librarySources)
	a.libraryDirty = true
	a.libraryRevision++
	persist := done || len(items) > 0 && (source == sourceHongguo || paginatedProvider(source)) || time.Since(a.libraryLastSave) >= 3*time.Second
	if persist {
		a.libraryLastSave = time.Now()
	}
	a.mu.Unlock()
	if persist {
		return a.persistLibrary()
	}
	return nil
}

func librarySourceErrors(sources map[string]librarySourceState) string {
	var messages []string
	for source, state := range sources {
		if state.Error != "" {
			messages = append(messages, fmt.Sprintf("%s 获取失败: %s", source, state.Error))
		}
	}
	sort.Strings(messages)
	return strings.Join(messages, "\n")
}

func cloneLibrarySources(sources map[string]librarySourceState) map[string]librarySourceState {
	copy := make(map[string]librarySourceState, len(sources))
	for source, state := range sources {
		copy[source] = state
	}
	return copy
}

func (a *UIApp) persistLibrary() error {
	a.libraryWriteMu.Lock()
	defer a.libraryWriteMu.Unlock()
	a.mu.Lock()
	revision := a.libraryRevision
	err := a.persistLibraryLocked()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.librarySaved = true
	if revision == a.libraryRevision {
		a.libraryDirty = false
	}
	return nil
}

// persistLibraryLocked 在已持有 a.mu 时落盘：只写改动过的分片与索引。
func (a *UIApp) persistLibraryLocked() error {
	meta := &libraryIndexFile{
		LoadedAt:   a.loadedAt,
		LastError:  librarySourceErrors(a.librarySources),
		Sources:    cloneLibrarySources(a.librarySources),
		Hongguo:    cloneHongguoCatalogState(a.libraryApp),
		Catalog:    cloneProviderCatalog(a.libraryProviders),
	}
	err := a.appLibrary().Persist(a.cfg.dataDirectory(), meta)
	if err != nil {
		a.libraryError = strings.TrimSpace(meta.LastError + "\n剧库缓存保存失败: " + a.redactError(err))
		a.libraryRevision++
	}
	return err
}

// librarySnapshotLocked 返回快照头字段与索引条目。剧集数据以条目形式返回，
// 由 writeLibrarySnapshot 逐条流式编码，避免一次性构造几十 MB 的响应体与 []Drama。
// 条目为 nil 表示自 revision 以来没有变化，响应里不会出现 data 字段。
func (a *UIApp) librarySnapshotLocked(revision uint64) (map[string]any, []dramaEntry) {
	// 复制一份条目切片：快照会在释放 a.mu 之后编码写出，不能持有库内部的活切片
	entries := append([]dramaEntry(nil), a.libraryEntries()...)
	remaining := sortMetadataRemainingEntries(entries)
	unqueued := map[string]int{"": 0, "hongguo": 0, "huangdou": 0, "huangguo": 0}
	for i := range entries {
		if a.metadataPending[entries[i].ID] || !needsSortMetadataEntry(entries[i]) {
			continue
		}
		source := entryProvider(entries[i])
		if source == sourceHuangguoAI || source == sourceHuangguoVideo {
			source = "huangguo"
		}
		unqueued[source]++
		unqueued[""]++
	}
	more := map[string]bool{}
	for source, count := range unqueued {
		more[source] = count > 0
	}
	more["hongguo"] = more["hongguo"] || hongguoCatalogHasMore(a.libraryApp)
	more[""] = more[""] || more["hongguo"]
	for source, cursor := range a.libraryProviders {
		base := providerCatalogSource(source)
		more[base] = more[base] || cursor.Initialized && !cursor.Exhausted
		more[""] = more[""] || more[base]
	}
	response := map[string]any{
		"revision": a.libraryRevision, "loadedAt": a.loadedAt, "loading": a.libraryLoading != nil,
		"cached": a.librarySaved && !a.libraryDirty, "error": a.libraryError,
		"sources": cloneLibrarySources(a.librarySources), "total": a.libraryLen(),
		"hasMore": more[""], "hasMoreBySource": more, "metadataRemaining": remaining,
		"loadingMore": a.libraryMore && a.libraryLoading != nil, "loadingSource": a.libraryLoadingSource, "loadingPhase": a.libraryLoadingPhase, "metadata": a.libraryMetadata,
	}
	if revision != 0 && revision == a.libraryRevision {
		response["unchanged"] = true
		return response, nil
	}
	return response, entries
}
