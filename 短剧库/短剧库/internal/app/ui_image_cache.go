package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

// 封面内存缓存上限。
//
// 响应本身已经带 private, max-age=86400，重复访问由浏览器消化；服务端留 8 MiB
// 只是为了让同一屏的并发请求合并（pending 去重）并少吃一次回源，再多就是纯浪费。
const coverCacheLimit = 8 << 20

// 缓存条目上限：平均一张封面约 40 KB，512 条会远超字节上限，实际由字节数主导。
const coverCacheEntries = 256

type coverCacheEntry struct {
	data     []byte
	expires  time.Time
	lastUsed time.Time
}

type coverImageJob struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	finished bool
	data     []byte
	err      error
}

type coverImageCache struct {
	mu      sync.Mutex
	entries map[string]*coverCacheEntry
	pending map[string]*coverImageJob
	slots   chan struct{}
	size    int
}

func (cache *coverImageCache) load(ctx context.Context, key string, fetch func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cache.mu.Lock()
	if entry := cache.entries[key]; entry != nil && time.Now().Before(entry.expires) {
		entry.lastUsed = time.Now()
		cache.mu.Unlock()
		return entry.data, nil
	}
	if cache.pending == nil {
		cache.entries = make(map[string]*coverCacheEntry)
		cache.pending = make(map[string]*coverImageJob)
		cache.slots = make(chan struct{}, 4)
	}
	job := cache.pending[key]
	if job == nil {
		if len(cache.pending) >= 128 {
			cache.mu.Unlock()
			return nil, errors.New("封面请求过多，请稍后重试")
		}
		workCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		job = &coverImageJob{done: make(chan struct{}), cancel: cancel}
		cache.pending[key] = job
		go cache.run(workCtx, key, job, fetch)
	}
	job.waiters++
	cache.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-job.done:
	}
	cache.mu.Lock()
	job.waiters--
	if !job.finished && job.waiters == 0 {
		job.cancel()
		if cache.pending[key] == job {
			delete(cache.pending, key)
		}
	}
	data, err := job.data, job.err
	cache.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}

func (cache *coverImageCache) run(ctx context.Context, key string, job *coverImageJob, fetch func(context.Context) ([]byte, error)) {
	defer job.cancel()
	var data []byte
	var err error
	select {
	case cache.slots <- struct{}{}:
		data, err = fetch(ctx)
		<-cache.slots
	case <-ctx.Done():
		err = ctx.Err()
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	job.data, job.err, job.finished = data, err, true
	if cache.pending[key] == job {
		delete(cache.pending, key)
		if err == nil && ctx.Err() == nil && len(data) > 0 && len(data) <= 4<<20 {
			cache.store(key, data)
		}
	}
	close(job.done)
}

func (cache *coverImageCache) store(key string, data []byte) {
	now := time.Now()
	for name, entry := range cache.entries {
		if name == key || !now.Before(entry.expires) {
			cache.size -= len(entry.data)
			delete(cache.entries, name)
		}
	}
	for cache.size+len(data) > coverCacheLimit || len(cache.entries) >= coverCacheEntries {
		oldest := ""
		var lastUsed time.Time
		for name, entry := range cache.entries {
			if oldest == "" || entry.lastUsed.Before(lastUsed) {
				oldest, lastUsed = name, entry.lastUsed
			}
		}
		cache.size -= len(cache.entries[oldest].data)
		delete(cache.entries, oldest)
	}
	cache.entries[key] = &coverCacheEntry{data: data, expires: now.Add(24 * time.Hour), lastUsed: now}
	cache.size += len(data)
}
