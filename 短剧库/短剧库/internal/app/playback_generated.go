package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type playbackSegmentIndex struct {
	Name     string
	Start    float64
	Duration float64
}

type playbackOutputError struct{ cause error }

func (err *playbackOutputError) Error() string { return err.cause.Error() }
func (err *playbackOutputError) Unwrap() error { return err.cause }

type playbackGenerated struct {
	ctx       context.Context
	cancel    context.CancelFunc
	directory string
	base      string
	query     string
	media     providerMedia
	playlist  string
	segments  []playbackSegmentIndex
	files     map[string]bool
	bytes     int64
	resources *playbackResources
	// done 在 ffmpeg 退出且结果落定后关闭；result 是落定结果（null 表示可播）。
	done   chan struct{}
	result error
	// failed 表示封装彻底失败且没有可用分片，ServeHTTP 直接回错让播放器重试。
	failed bool
	once   sync.Once
	mu     sync.RWMutex
}

// playbackGenerateFirstFrameWait 是允许生成流水线占住 plan 请求的最长时间。
// 只要首个分片落盘就立刻返回，让播放器边生成边播；超时也先返回，剩余分片后台继续。
const playbackGenerateFirstFrameWait = 2500 * time.Millisecond

const playbackGeneratedMIME = "application/vnd.apple.mpegurl"

// playbackGeneratedEmptyPlaylist 是首个分片还没落盘时下发的最小合法清单。
// 声明为 EVENT 且没有分片，播放器会按 live 规则重试拉取，而不是直接判为坏流。
const playbackGeneratedEmptyPlaylist = "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n"

func playbackGeneratedArgs(media providerMedia, input, processing string) []string {
	args := playbackInputArgs(media, input, 0)
	if processing == "audio" {
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2")
	} else {
		args = append(args, "-c", "copy")
	}
	return append(args, "-avoid_negative_ts", "make_zero", "-f", "hls", "-hls_time", "2", "-hls_list_size", "0",
		"-hls_playlist_type", "vod", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
		"-hls_flags", "temp_file", "-hls_segment_filename", "%06d.m4s", "index.m3u8")
}

// generatePlayback 启动媒体封装，并在首个分片可用时返回，让播放器立即起播。
//
// 封装本身是 -c copy，产物落在临时目录里供按需拖动；只要第一个分片落盘就能出首帧，
// 不需要等整集跑完。ffmpeg 仍在跑时 ServeHTTP 会把正在增长的 index.m3u8 改写成对外
// 播放清单（注入 EXT-X-START 让播放器从 0 开始），跑完后再切换成校验过的完整 VOD 清单。
func (app *UIApp) generatePlayback(ctx context.Context, media *playbackMediaSession, processing, base, query string) error {
	ffmpeg, err := app.downloader.ensureFFmpeg(ctx)
	if err != nil {
		return err
	}
	release, err := app.mediaResources().acquire(ctx, processing, false)
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "juku-playback-media-")
	if err != nil {
		release()
		return err
	}
	jobCtx, cancel := context.WithCancel(media.ctx)
	// jobCtx 故意不再挂 ctx：plan 请求返回后 ctx 会被调用方 cancel，
	// 而封装必须继续在后台跑完，否则「边生成边播」当场断流。
	// 生命周期统一由 media.Close()/会话结束走 cancel 回收。
	generated := &playbackGenerated{ctx: jobCtx, cancel: cancel, directory: directory, base: base, query: query,
		media: media.media, resources: app.mediaResources(), done: make(chan struct{}), files: make(map[string]bool)}
	media.generated = generated
	input := media.local
	var proxy *hlsProxy
	if input == "" {
		proxy, err = app.downloader.newHLSProxy(jobCtx, media.media, media.key)
		if err != nil {
			generated.Close()
			release()
			return err
		}
		proxy.acquire = func(ctx context.Context) (func(), error) { return app.mediaResources().acquire(ctx, "media", false) }
		input = proxy.root
	} else {
		input, err = filepath.Abs(input)
		if err != nil {
			generated.Close()
			release()
			return err
		}
		input = filepath.ToSlash(input)
	}
	command := ffmpegMediaCommand(jobCtx, ffmpeg, playbackGeneratedArgs(media.media, input, processing)...)

	command.Dir = directory
	log := &playbackLog{text: cappedStringWriter{limit: 64 * 1024}, ready: make(chan struct{})}
	command.Stderr = log
	if err := command.Start(); err != nil {
		if proxy != nil {
			proxy.Close()
		}
		generated.Close()
		release()
		return fmt.Errorf("无法启动媒体封装：%w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	// 收尾协程独占 ffmpeg 的生命周期：结算磁盘缓存、落定最终清单、关闭输入代理、
	// 释放播放额度。plan 提前返回后这些事仍然要跑完，所以不能挂在请求的 defer 上。
	go func() {
		defer release()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		// 只处理一次取消，之后把该分支置空，避免在已关闭的 channel 上空转。
		cancelled := jobCtx.Done()
		for {
			select {
			case commandErr := <-exited:
				generated.complete(commandErr, proxy, log)
				if proxy != nil {
					proxy.Close()
				}
				return
			case <-ticker.C:
				if err := generated.accountCache(); err != nil {
					cancel()
				}
			case <-cancelled:
				cancel()
				cancelled = nil
			}
		}
	}()

	// plan 立刻指向生成目录：播放器拿到第一个分片就能起播。
	media.plan.URL = generated.assetURL("index.m3u8")
	media.plan.Player, media.plan.MIME, media.plan.Processing = "hls", playbackGeneratedMIME, processing
	if media.plan.Duration > 0 {
		media.plan.SeekEnd = media.plan.Duration
	}
	deadline := time.Now().Add(playbackGenerateFirstFrameWait)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-generated.done:
			return generated.result
		case <-ctx.Done():
			cancel()
			<-generated.done
			return ctx.Err()
		case <-ticker.C:
			if generated.playable() || time.Now().After(deadline) {
				return nil
			}
		}
	}
}

// playable 表示已经至少有一个分片可供起播。
func (generated *playbackGenerated) playable() bool {
	body, err := os.ReadFile(filepath.Join(generated.directory, "index.m3u8"))
	return err == nil && strings.Contains(string(body), ".m4s")
}

// complete 在 ffmpeg 退出后落定结果：成功就校验完整清单，失败就记录原因。
func (generated *playbackGenerated) complete(commandErr error, proxy *hlsProxy, log *playbackLog) {
	generated.mu.Lock()
	defer func() {
		generated.mu.Unlock()
		close(generated.done)
	}()
	if commandErr != nil {
		generated.result = playbackStreamError(generated.ctx, proxy, log, generated.media, commandErr, nil)
		// 一个分片都没产出：直说失败，让播放器立刻重试，而不是对着空清单干等。
		if !generated.playable() {
			generated.failed = true
		}
		return
	}
	if err := generated.accountCacheLocked(); err != nil {
		generated.result = err
		return
	}
	body, err := os.ReadFile(filepath.Join(generated.directory, "index.m3u8"))
	if err != nil {
		generated.result = &playbackOutputError{fmt.Errorf("媒体播放清单未生成：%w", err)}
		return
	}
	if err := generated.index(string(body)); err != nil {
		generated.result = err
		return
	}
}

// accountCache 统计临时目录占用并计入磁盘缓存配额（自带加锁，供外部调用）。
func (generated *playbackGenerated) accountCache() error {
	generated.mu.Lock()
	defer generated.mu.Unlock()
	return generated.accountCacheLocked()
}

// accountCacheLocked 统计临时目录占用并计入磁盘缓存配额；调用方必须持有 generated.mu。
func (generated *playbackGenerated) accountCacheLocked() error {
	entries, err := os.ReadDir(generated.directory)
	if err != nil {
		return err
	}
	var size int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 64<<20 {
			return errors.New("媒体分片超出缓存限制")
		}
		size += info.Size()
	}
	resources := generated.resources
	resources.mu.Lock()
	defer resources.mu.Unlock()
	if resources.cacheBytes+size-generated.bytes > int64(resources.settings.CacheMB)<<20 {
		return errors.New("媒体临时缓存已达上限，请关闭其他播放后重试")
	}
	resources.cacheBytes += size - generated.bytes
	generated.bytes = size
	return nil
}

func (generated *playbackGenerated) assetURL(name string) string {
	address := generated.base + name
	if generated.query != "" {
		address += "?" + generated.query
	}
	return address
}

func validPlaybackSegmentName(name string) bool {
	return name != "" && filepath.Base(name) == name && !strings.ContainsAny(name, "\\?\x00") &&
		(strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".m4s"))
}

func (generated *playbackGenerated) index(playlist string) (err error) {
	defer func() {
		if err != nil {
			err = &playbackOutputError{err}
		}
	}()
	if len(playlist) > 2<<20 || !strings.HasPrefix(playlist, "#EXTM3U") || !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		return errors.New("媒体封装尚未完成，不能生成完整播放索引")
	}
	lines := strings.Split(playlist, "\n")
	var start, duration, longest float64
	for i, line := range lines {
		if strings.HasPrefix(line, "#EXTINF:") {
			value, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			var err error
			duration, err = strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 3600 {
				return errors.New("媒体分片时长无效")
			}
			longest = math.Max(longest, duration)
		} else if strings.HasPrefix(line, "#EXT-X-MAP:") {
			matches := hlsURIAttribute.FindStringSubmatch(line)
			if len(matches) != 2 || !validPlaybackSegmentName(matches[1]) {
				return errors.New("媒体初始化片段无效")
			}
			generated.files[matches[1]] = true
			lines[i] = strings.Replace(line, matches[0], `URI="`+generated.assetURL(matches[1])+`"`, 1)
		} else if line != "" && !strings.HasPrefix(line, "#") {
			if duration <= 0 || !validPlaybackSegmentName(line) || len(generated.segments) >= 16384 || start+duration > 24*60*60 {
				return errors.New("媒体分片索引无效")
			}
			generated.files[line] = true
			generated.segments = append(generated.segments, playbackSegmentIndex{Name: line, Start: start, Duration: duration})
			start += duration
			duration = 0
			lines[i] = generated.assetURL(line)
		}
	}
	if len(generated.segments) == 0 || duration != 0 {
		return errors.New("媒体播放索引没有完整分片")
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			lines[i] = "#EXT-X-TARGETDURATION:" + strconv.Itoa(int(math.Ceil(longest)))
		}
	}
	for name := range generated.files {
		info, err := os.Stat(filepath.Join(generated.directory, name))
		if err != nil {
			return fmt.Errorf("媒体分片未完整生成（%s）：%w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("媒体分片未完整生成（%s 不是普通文件）", name)
		}
		if info.Size() <= 0 {
			return fmt.Errorf("媒体分片未完整生成（%s 为空文件）", name)
		}
	}
	generated.playlist = strings.Join(lines, "\n")
	return nil
}

// progressivePlaylist 把 ffmpeg 正在增长的 index.m3u8 改写成对外播放清单。
// 未完成时清单里没有 ENDLIST（播放器会按 live 轮询），并注入 EXT-X-START
// 让它从 0 开始而不是从 live 边缘开始。
func (generated *playbackGenerated) progressivePlaylist() string {
	body, err := os.ReadFile(filepath.Join(generated.directory, "index.m3u8"))
	if err != nil {
		return ""
	}
	if len(body) > 2<<20 {
		return ""
	}
	lines := strings.Split(string(body), "\n")
	out := make([]string, 0, len(lines)+1)
	started := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "#EXT-X-MAP:"):
			if matches := hlsURIAttribute.FindStringSubmatch(trimmed); len(matches) == 2 && validPlaybackSegmentName(matches[1]) {
				trimmed = strings.Replace(trimmed, matches[0], `URI="`+generated.assetURL(matches[1])+`"`, 1)
			}
		case strings.HasPrefix(trimmed, "#EXT-X-START:"):
			continue
		case strings.HasPrefix(trimmed, "#EXT-X-PLAYLIST-TYPE:"):
			// ffmpeg 一直写 VOD；未写完就声明 VOD 会让播放器按点播处理、不再轮询。
			trimmed = "#EXT-X-PLAYLIST-TYPE:EVENT"
		case !strings.HasPrefix(trimmed, "#"):
			if !validPlaybackSegmentName(trimmed) {
				continue
			}
			trimmed = generated.assetURL(trimmed)
		case !started && strings.HasPrefix(trimmed, "#EXT-X-TARGETDURATION:"):
			out = append(out, trimmed, "#EXT-X-START:TIME-OFFSET=0")
			started = true
			continue
		}
		out = append(out, trimmed)
	}
	// ffmpeg 是原地重写清单的，读到的可能是写了一半的文件；丢掉末尾悬空的标签，
	// 免得给播放器一个「有 EXTINF 没有分片」的清单。
	for len(out) > 1 {
		last := out[len(out)-1]
		if !strings.HasPrefix(last, "#") || strings.HasPrefix(last, "#EXTM3U") {
			break
		}
		out = out[:len(out)-1]
	}
	if len(out) <= 1 || !strings.HasPrefix(out[0], "#EXTM3U") {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func (generated *playbackGenerated) Close() {
	generated.once.Do(func() {
		generated.cancel()
		go func() {
			<-generated.done
			generated.mu.Lock()
			defer generated.mu.Unlock()
			_ = os.RemoveAll(generated.directory)
			generated.resources.mu.Lock()
			generated.resources.cacheBytes -= generated.bytes
			generated.resources.mu.Unlock()
		}()
	})
}

func (generated *playbackGenerated) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	generated.mu.RLock()
	defer generated.mu.RUnlock()
	if generated.ctx.Err() != nil || generated.failed {
		writer.WriteHeader(http.StatusGone)
		return
	}
	name := strings.TrimPrefix(request.URL.Path, generated.base)
	if name == "index.m3u8" {
		text := generated.playlist
		if text == "" {
			// 还在生成：给一份此刻可播的清单，播放器边下边播。
			text = generated.progressivePlaylist()
		}
		if text == "" {
			text = playbackGeneratedEmptyPlaylist
		}
		writer.Header().Set("Content-Type", playbackGeneratedMIME)
		writer.Header().Set("Cache-Control", "no-store")
		http.ServeContent(writer, request, name, time.Time{}, strings.NewReader(text))
		return
	}
	if !generated.files[name] && !validPlaybackSegmentName(name) {
		// 渐进阶段 files 还没登记，只要名字合法就按文件是否存在决定。
		http.NotFound(writer, request)
		return
	}
	file, err := os.Open(filepath.Join(generated.directory, name))
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(writer, request, name, time.Time{}, file)
}
