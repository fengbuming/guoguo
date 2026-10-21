package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const playbackPrefetchChunkSize = 64 * 1024
const playbackPrefetchChunks = 512

type playbackPrefetchKey struct{}

type playbackPrefetch struct {
	episode    int
	fromRun    uint64
	quality    int
	remux      bool
	planOnly   bool
	prepare    string
	resolved   *playbackMediaSession
	generated  *playbackGenerated
	native     *playbackNative
	ctx        context.Context
	cancel     context.CancelFunc
	chunks     chan []byte
	ready      chan struct{}
	done       chan struct{}
	once       sync.Once
	headers    http.Header
	status     int
	mu         sync.Mutex
	header     http.Header
	err        error
	foreground atomic.Bool
	preempted  bool
}

type playbackPrefetchView struct {
	Episode int    `json:"episode"`
	State   string `json:"state"`
}

func newPlaybackPrefetch(episode int, run uint64) *playbackPrefetch {
	ctx := context.WithValue(context.Background(), backgroundCatalogKey{}, true)
	cache := &playbackPrefetch{episode: episode, fromRun: run,
		chunks: make(chan []byte, playbackPrefetchChunks), ready: make(chan struct{}), done: make(chan struct{}), headers: make(http.Header)}
	ctx = context.WithValue(ctx, playbackPrefetchKey{}, cache)
	ctx, cancel := context.WithCancel(ctx)
	base := cancel
	cache.ctx, cache.cancel = ctx, func() {
		base()
		cache.releasePrepared()
	}
	return cache
}

// takeGenerated 把预生成的封装产物交给前台会话；交出去之后预取不再负责关闭它。
func (cache *playbackPrefetch) takeGenerated() *playbackGenerated {
	cache.mu.Lock()
	generated := cache.generated
	cache.generated = nil
	cache.mu.Unlock()
	return generated
}

// releasePrepared 预取被丢弃时关掉还没人接手的产物。
func (cache *playbackPrefetch) releasePrepared() {
	if generated := cache.takeGenerated(); generated != nil {
		generated.Close()
	}
}

func (cache *playbackPrefetch) Header() http.Header              { return cache.headers }
func (cache *playbackPrefetch) WriteHeader(status int)           { cache.status = status }
func (cache *playbackPrefetch) Flush()                           {}
func (cache *playbackPrefetch) SetWriteDeadline(time.Time) error { return nil }

func (cache *playbackPrefetch) Write(data []byte) (int, error) {
	if cache.status == 0 {
		cache.status = http.StatusOK
	}
	if cache.status != http.StatusOK {
		return len(data), nil
	}
	cache.once.Do(func() {
		cache.mu.Lock()
		cache.header = cache.headers.Clone()
		cache.mu.Unlock()
		close(cache.ready)
	})
	written := 0
	for len(data) > 0 {
		size := len(data)
		if size > playbackPrefetchChunkSize {
			size = playbackPrefetchChunkSize
		}
		chunk := append([]byte(nil), data[:size]...)
		select {
		case cache.chunks <- chunk:
			data = data[size:]
			written += size
		case <-cache.ctx.Done():
			return written, cache.ctx.Err()
		}
	}
	return written, nil
}

func (cache *playbackPrefetch) finish(err error) {
	cache.mu.Lock()
	cache.err = err
	cache.mu.Unlock()
	close(cache.chunks)
	close(cache.done)
	cache.once.Do(func() { close(cache.ready) })
}

func (cache *playbackPrefetch) view() *playbackPrefetchView {
	cache.mu.Lock()
	prepared := cache.generated != nil && cache.err == nil
	// 会话已经解析、产物还在生成：对前端来说是"还在准备"，不能报成 resolved。
	preparing := cache.prepare == "remux" && cache.generated == nil && cache.err == nil
	resolved := cache.resolved != nil && cache.err == nil && !preparing
	cache.mu.Unlock()
	if prepared {
		return &playbackPrefetchView{Episode: cache.episode, State: "prepared"}
	}
	if resolved {
		return &playbackPrefetchView{Episode: cache.episode, State: "resolved"}
	}
	if cache.native != nil {
		state, _ := cache.native.state()
		switch state {
		case "ended":
			state = "ready"
		case "failed", "stopped":
			state = "failed"
		case "streaming":
			state = "buffering"
			select {
			case <-cache.native.firstBatch:
				state = "partial"
			default:
			}
		default:
			state = "preparing"
		}
		return &playbackPrefetchView{Episode: cache.episode, State: state}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	state := "preparing"
	if cache.err != nil {
		state = "failed"
	} else if cache.header != nil {
		state = "buffering"
		select {
		case <-cache.done:
			state = "ready"
		default:
		}
	}
	return &playbackPrefetchView{Episode: cache.episode, State: state}
}

func (cache *playbackPrefetch) serve(ctx context.Context, writer http.ResponseWriter, run uint64, ready func(float64)) (bool, error) {
	select {
	case <-cache.ready:
	case <-ctx.Done():
		return true, ctx.Err()
	}
	cache.mu.Lock()
	header, failed := cache.header, cache.err
	cache.mu.Unlock()
	if failed != nil || header == nil {
		return false, nil
	}
	var first []byte
	select {
	case chunk, ok := <-cache.chunks:
		if !ok {
			return false, nil
		}
		first = chunk
	case <-ctx.Done():
		return true, ctx.Err()
	}
	for _, name := range []string{"Content-Type", "Content-Disposition", "X-Playback-Duration", "X-Playback-Source", "X-Playback-MIME", "X-Playback-Mode", "X-Playback-Quality", "X-Playback-Qualities"} {
		if value := header.Get(name); value != "" {
			writer.Header().Set(name, value)
		}
	}
	writer.Header().Set("X-Playback-Run", strconv.FormatUint(run, 10))
	writer.Header().Set("X-Playback-Prefetched", "1")
	duration, _ := strconv.ParseFloat(header.Get("X-Playback-Duration"), 64)
	ready(duration)
	controller := http.NewResponseController(writer)
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()); close(interrupted) })
	defer func() {
		if !stopInterrupt() {
			<-interrupted
		}
		_ = controller.SetWriteDeadline(time.Time{})
	}()
	for {
		if _, err := writer.Write(first); err != nil {
			return true, err
		}
		if err := controller.Flush(); err != nil {
			return true, err
		}
		select {
		case chunk, ok := <-cache.chunks:
			if !ok {
				<-cache.done
				cache.mu.Lock()
				err := cache.err
				cache.mu.Unlock()
				return true, err
			}
			first = chunk
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
}

func (app *UIApp) handlePlaybackPrefetch(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Session string `json:"session"`
		Episode int    `json:"episode"`
		Run     uint64 `json:"run"`
		Version uint64 `json:"version"`
		Cancel  bool   `json:"cancel"`
		Remux   bool   `json:"remux"`
		// Prepare 为空表示只解析地址；"remux" 表示直接把下一集封装成可按需拖动的产物，
		// 切集时 plan 直接接管，省掉 2~3 秒的 ffmpeg 启动等待。
		Prepare string `json:"prepare"`
	}
	if !readPlaybackRequest(writer, request, &input) {
		return
	}
	if input.Prepare != "" && input.Prepare != "remux" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "预取方式无效"})
		return
	}
	app.playbackMu.Lock()
	session := app.playbacks[input.Session]
	if !viewerOwnsPlayback(request.Context(), session) {
		app.playbackMu.Unlock()
		writeJSON(writer, http.StatusGone, map[string]string{"error": "播放会话已过期"})
		return
	}
	if input.Run != session.run || input.Version == 0 || input.Version < session.prefetchVersion {
		app.playbackMu.Unlock()
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "播放分集已变化"})
		return
	}
	if !input.Cancel && (input.Episode < 1 || input.Episode != session.currentIndex+1 || input.Episode > len(session.tasks)) {
		app.playbackMu.Unlock()
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "只能预缓存当前集的下一集"})
		return
	}
	state := session.state
	if session.native != nil {
		state, _ = session.native.state()
	}
	// 转码 / native 模式下等「整集缓冲结束」才预取等于不预取（要等整集下载转码完），
	// 因此只要当前集已经出片、用户能正常播放，就放行下一集的后台预取。
	nativeReady := session.native != nil && session.native.prefetchReady()
	if !input.Cancel && state != "ended" && session.media == nil && !nativeReady {
		app.playbackMu.Unlock()
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "当前集尚未缓冲完成"})
		return
	}
	session.prefetchVersion = input.Version
	app.touchPlaybackLocked(session)
	previous := session.prefetch
	if input.Cancel {
		session.prefetch = nil
		app.playbackMu.Unlock()
		if previous != nil {
			previous.cancel()
		}
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	remux := input.Remux && session.native == nil
	if previous != nil && previous.episode == input.Episode && previous.fromRun == input.Run && previous.remux == remux {
		view := previous.view()
		app.playbackMu.Unlock()
		writeJSON(writer, http.StatusOK, view)
		return
	}
	cache := newPlaybackPrefetch(input.Episode, input.Run)
	cache.quality, cache.remux = session.quality, remux
	cache.prepare = input.Prepare
	// MSE / 原始直连模式只做地址解析（planOnly）：这两种模式播放时字节本来就要经过
	// 服务端代理，预取的价值在于提前完成取址/解密，让连播切换秒开；真缓存 32MB
	// 反而白白占用一倍带宽。native HLS 模式（手机）则真正缓存首批分片，因为
	// native 管线启动慢（下载+转码），提前缓存对切集流畅度收益最大。
	// 预取 acquire 是 optional（前台播放占用资源时自动跳过，不会拖慢当前集）。
	cache.planOnly = session.media != nil && session.native == nil
	cache.ctx = context.WithValue(cache.ctx, playbackQualityKey{}, cache.quality)
	cache.ctx = context.WithValue(cache.ctx, playbackRemuxKey{}, remux)
	session.prefetch = cache
	task := session.tasks[input.Episode-1]
	downloadID := ""
	if len(session.downloadIDs) > 0 {
		downloadID = session.downloadIDs[input.Episode-1]
	}
	if session.native != nil {
		cache.native = newPlaybackNative(app, cache.ctx, task, downloadID, cache.quality, 0, true)
		cancel := cache.cancel
		cache.cancel = func() { cancel(); cache.native.Close() }
	}
	app.playbackMu.Unlock()
	if previous != nil {
		previous.cancel()
	}
	go func() {
		release, err := app.mediaResources().acquire(cache.ctx, "prefetch", true)
		if err != nil {
			cache.cancel()
			cache.finish(err)
			return
		}
		defer release()
		startup := time.AfterFunc(60*time.Second, cache.cancel)
		defer startup.Stop()
		if cache.planOnly {
			resolved, err := app.resolveMediaSession(cache.ctx, cache.ctx, task, downloadID, cache.quality)
			if err != nil {
				cache.cancel()
				cache.finish(err)
				return
			}
			defer resolved.Close()
			cache.mu.Lock()
			cache.resolved = resolved
			cache.mu.Unlock()
			if cache.prepare == "remux" {
				// 前台正在播这一集时提前把下一集封装好：切集时 plan 直接接管产物，
				// 不再等 ffmpeg 启动 + 拉首片。产物用「不随预取取消」的上下文，
				// 否则预取一收尾 ffmpeg 就被掐，接管等于白做。
				generated, err := app.preparePlaybackGenerated(cache, resolved)
				if err == nil {
					cache.mu.Lock()
					cache.generated = generated
					cache.mu.Unlock()
				}
				app.downloader.recordDiagnostic(diagnosticEvent{Level: "info", Event: "playback.prefetch.prepare",
					Episode: cache.episode, Message: app.redactError(err)})
				cache.finish(err)
				return
			}
			app.downloader.recordDiagnostic(diagnosticEvent{Level: "info", Event: "playback.prefetch.resolve",
				Episode: cache.episode, Message: "planOnly prepare=" + cache.prepare})
			cache.cancel()
			cache.finish(nil)
			return
		}
		if cache.native != nil {
			cache.native.start()
			_, err := cache.native.segment(cache.ctx, 0)
			startup.Stop()
			if err == nil {
				select {
				case <-cache.native.firstBatch:
					_, err = cache.native.state()
				case <-cache.ctx.Done():
					err = cache.ctx.Err()
				}
			}
			if err != nil {
				cache.cancel()
			}
			cache.finish(err)
			return
		}
		defer cache.cancel()
		err = app.streamPlayback(cache.ctx, cache.cancel, cache, task, downloadID, 0, 0, func(float64) { startup.Stop() })
		cache.finish(err)
	}()
	writeJSON(writer, http.StatusAccepted, &playbackPrefetchView{Episode: input.Episode, State: "preparing"})
}

// preparePlaybackGenerated 在预取阶段把下一集的封装产物做出来。
//
// 只对「服务端解密 + 换封装」这一类预生成：非加密源要先探测编码，结论不确定，
// 预生成可能白跑一遍 ffmpeg。产物用不随预取取消的上下文，切集时由 plan 接管。
func (app *UIApp) preparePlaybackGenerated(cache *playbackPrefetch, media *playbackMediaSession) (*playbackGenerated, error) {
	if len(media.media.CENCKey) == 0 {
		return nil, errors.New("只有加密源需要预生成封装产物")
	}
	if err := app.generatePlaybackWithParent(cache.ctx, context.WithoutCancel(cache.ctx), media, "remux", "", ""); err != nil {
		return nil, err
	}
	generated := media.generated
	// 交出去：resolved 会话随预取收尾关闭，不能再让它把产物一起关掉。
	media.generated = nil
	if generated == nil {
		return nil, errors.New("预生成封装产物失败")
	}
	return generated, nil
}

func (app *UIApp) servePrefetchedPlayback(ctx context.Context, writer http.ResponseWriter, cache *playbackPrefetch, run uint64, ready func(float64)) (bool, error) {	if cache == nil {
		return false, nil
	}
	used, err := cache.serve(ctx, writer, run, ready)
	if !used {
		cache.cancel()
	}
	return used, err
}

var _ http.ResponseWriter = (*playbackPrefetch)(nil)
