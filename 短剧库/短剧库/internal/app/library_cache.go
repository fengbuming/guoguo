package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type libraryCache struct {
	Dramas          []Drama                          `json:"dramas"`
	LoadedAt        time.Time                        `json:"loadedAt"`
	LastError       string                           `json:"lastError,omitempty"`
	Sources         map[string]librarySourceState    `json:"sources,omitempty"`
	HongguoApp      *hongguoCatalogState             `json:"hongguoApp,omitempty"`
	ProviderCatalog map[string]providerCatalogCursor `json:"providerCatalog,omitempty"`
}

func libraryCachePath(outputDir string) string {
	return filepath.Join(outputDir, "library.json")
}

// readLibraryCache 优先读取分片剧库（UI 现在的落盘位置），没有时才回落到旧版整文件缓存，
// 保证 CLI、同步包与既有调用方看到的都是最新数据。
func readLibraryCache(outputDir string) (libraryCache, error) {
	var cache libraryCache
	if cache, err := readLibraryStoreCache(outputDir); err == nil {
		return cache, nil
	}
	path := libraryCachePath(outputDir)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		path = filepath.Join(outputDir, "ui-state.json")
		body, err = os.ReadFile(path)
	}
	if err != nil {
		return cache, err
	}
	if err := json.Unmarshal(body, &cache); err != nil {
		return cache, fmt.Errorf("本地剧库缓存损坏 %s: %w", path, err)
	}
	return cache, nil
}

// readLibraryStoreCache 从分片剧库重建旧版缓存结构；剧库为空时返回错误以便回落到旧文件。
func readLibraryStoreCache(outputDir string) (libraryCache, error) {
	store, err := openLibraryStore(outputDir)
	if err != nil {
		return libraryCache{}, err
	}
	if store.Len() == 0 {
		return libraryCache{}, os.ErrNotExist
	}
	return libraryCache{
		Dramas:          store.All(),
		LoadedAt:        store.meta.LoadedAt,
		LastError:       store.meta.LastError,
		Sources:         store.meta.Sources,
		HongguoApp:      store.meta.Hongguo,
		ProviderCatalog: store.meta.Catalog,
	}, nil
}

// writeLibraryStoreCache 把整份剧库写入分片存储，供 CLI 刷新后同步给 UI 使用。
func writeLibraryStoreCache(outputDir string, cache libraryCache) error {
	if len(cache.Dramas) == 0 {
		return nil
	}
	store := &libraryStore{dir: outputDir, byID: map[string]int{}, shards: map[uint16]*libraryShard{}, dirty: map[uint16]bool{}}
	store.importDramas(cache.Dramas)
	meta := &libraryIndexFile{
		LoadedAt:  cache.LoadedAt,
		LastError: cache.LastError,
		Sources:   cache.Sources,
		Hongguo:   cache.HongguoApp,
		Catalog:   cache.ProviderCatalog,
	}
	return store.Persist(outputDir, meta)
}

func writeLibraryCache(outputDir string, cache libraryCache) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(outputDir, ".library-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(cache); err != nil {
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
	return os.Rename(file.Name(), libraryCachePath(outputDir))
}

func (a *UIApp) loadLibrary() {
	a.mu.Lock()
	defer a.mu.Unlock()
	store, storeErr := openLibraryStore(a.cfg.dataDirectory())
	if storeErr != nil {
		if !errors.Is(storeErr, os.ErrNotExist) {
			a.libraryError = a.redactError(storeErr)
		}
		return
	}
	a.library = store
	// 只有在磁盘剧库为空时才读取旧版整文件缓存，避免每次启动都解析几十 MB 的 JSON
	var cache libraryCache
	legacy := false
	if store.Len() == 0 {
		legacyCache, err := readLibraryCache(a.cfg.dataDirectory())
		legacy = err == nil && len(legacyCache.Dramas) > 0
		if legacy {
			cache = legacyCache
			a.downloader.restoreHongguoCatalog(cache.HongguoApp)
			a.downloader.restoreProviderCatalog(cache.ProviderCatalog)
			a.normalizeDramaCovers(cache.Dramas)
			a.setLibraryDramas(cache.Dramas)
			a.loadedAt = cache.LoadedAt
			a.libraryError = cache.LastError
		}
	}
	if store.hasMeta {
		a.loadedAt = store.meta.LoadedAt
		a.libraryError = store.meta.LastError
		a.librarySources = store.meta.Sources
		a.libraryProviders = store.meta.Catalog
		if store.meta.Hongguo != nil {
			a.libraryApp = store.meta.Hongguo
		}
	} else if legacy {
		a.librarySources = cache.Sources
		a.libraryProviders = a.downloader.providerCatalogSnapshot()
		if cache.HongguoApp != nil {
			a.libraryApp = a.downloader.hongguoCatalogSnapshot()
		}
	}
	for source, state := range a.librarySources {
		if state.Status == "loading" {
			state.Status = "partial"
			state.Error = "上次更新中断，当前显示已保存数据，可手动刷新"
			a.librarySources[source] = state
		}
	}
	if sourceErrors := librarySourceErrors(a.librarySources); sourceErrors != "" {
		a.libraryError = sourceErrors
	}
	a.libraryRevision = 1
	if a.libraryLen() > 0 {
		if !store.hasMeta {
			if err := a.persistLibraryLocked(); err != nil {
				a.libraryError = "剧库缓存保存失败: " + a.redactError(err)
				return
			}
		}
		a.librarySaved = true
		var pending []Drama
		for _, entry := range a.libraryEntries() {
			if entry.SortMetadata != nil && entry.SortMetadata.Pending {
				if drama, found := a.libraryDrama(entry.ID); found {
					pending = append(pending, drama)
				}
			}
		}
		a.enqueueSortMetadataLocked(pending, false)
		fmt.Printf("已恢复本地剧库：%d 部，索引：%s\n", a.libraryLen(), libraryIndexPath(a.cfg.dataDirectory()))
	}
	// 冷启动解析 index.json 是进程生命周期里最大的一次分配峰值，解析完立刻还给系统，
	// 否则空闲 RSS 会一直停在峰值附近。
	a.releaseRuntimeMemory()
}

func (d *Downloader) GetAllDramas(ctx context.Context) ([]Drama, error) {
	cache, err := readLibraryCache(d.cfg.dataDirectory())
	if err == nil {
		d.restoreHongguoCatalog(cache.HongguoApp)
		d.restoreProviderCatalog(cache.ProviderCatalog)
	}
	if err == nil && len(cache.Dramas) > 0 {
		fmt.Printf("使用本地剧库缓存：%d 部（%s），更新请使用 -refresh\n", len(cache.Dramas), cache.LoadedAt.Format("2006-01-02 15:04:05"))
		if cache.LastError != "" {
			return cache.Dramas, errors.New(cache.LastError)
		}
		return cache.Dramas, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Printf("  读取本地剧库缓存失败: %v\n", err)
	}
	return d.RefreshDramas(ctx, "")
}

func (d *Downloader) RefreshDramas(ctx context.Context, source string) ([]Drama, error) {
	cached, _ := readLibraryCache(d.cfg.dataDirectory())
	d.restoreHongguoCatalog(cached.HongguoApp)
	d.restoreProviderCatalog(cached.ProviderCatalog)
	ctx = withKnownHongguoDramas(ctx, cached.Dramas)
	fresh, loadErr := d.fetchAllDramas(ctx, source)
	merged := mergeSourceDramas(cached.Dramas, fresh, loadErr, source)
	if len(fresh) == 0 && len(merged) == 0 {
		return nil, loadErr
	}
	cache := libraryCache{Dramas: merged, LoadedAt: time.Now(), HongguoApp: d.hongguoCatalogSnapshot(), ProviderCatalog: d.providerCatalogSnapshot(), Sources: cached.Sources}
	if len(fresh) == 0 {
		cache.LoadedAt = cached.LoadedAt
	}
	if loadErr != nil {
		cache.LastError = publicError(loadErr).Error()
	}
	if err := writeLibraryCache(d.cfg.dataDirectory(), cache); err != nil {
		loadErr = errors.Join(loadErr, fmt.Errorf("剧库缓存保存失败: %w", err))
	}
	// 已存在分片剧库时同步写入，避免 CLI 刷新结果对 Web 界面不可见
	if _, err := os.Stat(libraryIndexPath(d.cfg.dataDirectory())); err == nil {
		if err := writeLibraryStoreCache(d.cfg.dataDirectory(), cache); err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("分片剧库保存失败: %w", err))
		}
	}
	fmt.Printf("本地剧库：%d 部，本次新增 %d 部\n", len(merged), len(merged)-len(cached.Dramas))
	return merged, loadErr
}
