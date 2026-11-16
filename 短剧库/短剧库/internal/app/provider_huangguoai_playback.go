package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 站点 2026-11-13 改版后的取址协议（依据播放页 services/playback.js）：
//   - 播放页 /video/<id>/ 的 videoInitialData 不再内联 videoSrc / epPlaySrcs，
//     旧 GET /api/videos/<id>/play?ep= 也只返回空 video_url；
//   - 现行取址为 POST /api/videos/<id>/episodes/<ep>/playback，请求体必须是空 JSON {}，
//     响应 data.streams.main 为带时效签名的 m3u8；首播、切集、恢复均重新 POST；
//   - 前端仅对 .m3u8 附加 x-cdn-auth / x-cdn-app，而清单内的 key 与 ts 均为
//     绝对签名地址，实测无需附加头即可读取，因此不改动既有媒体代理请求结构。
//
// 这里只替换「取址」这一步，旧页面解析 parseAIVideoURL 仍保留为回退路径，
// 片源解密、清晰度择优、续读换节点、缓存与播放优化均不触碰。

const huangguoAIPlaybackFallbackHost = "https://huangguoai.ai"

var (
	reHuangguoAIEpisodePath     = regexp.MustCompile(`/ep-(\d+)(?:[/?#]|$)`)
	errHuangguoAIPlaybackStatus = errors.New("huangguoai 取址被站点拒绝")
)

type huangguoAIPlaybackResponse struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Data   struct {
		ID             json.RawMessage `json:"id"`
		EP             json.RawMessage `json:"ep"`
		ManifestMethod string          `json:"manifest_method"`
		Streams        struct {
			Main    string `json:"main"`
			Hevc    string `json:"hevc"`
			Preview string `json:"preview"`
		} `json:"streams"`
		CDN struct {
			M3U8Enabled bool   `json:"m3u8Enabled"`
			AuthKey     string `json:"authKey"`
			AuthValue   string `json:"authValue"`
			App         string `json:"app"`
		} `json:"cdn"`
	} `json:"data"`
}

// huangguoAIEpisodeNumber 优先取播放页 URL 里的 /ep-N/，其次是章节自带的集号。
// 站点接口要求的是页内 1-based 集号，不能用任务下标猜测。
func huangguoAIEpisodeNumber(chapter Chapter, fallback int, pageURL string) int {
	if m := reHuangguoAIEpisodePath.FindStringSubmatch(pageURL); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	if n, err := strconv.Atoi(chapter.EpisodeString(fallback)); err == nil && n > 0 {
		return n
	}
	if fallback > 0 {
		return fallback
	}
	return 1
}

// huangguoAIPlaybackHosts 给出取址域名候选：配置线路、记录中最近成功的线路、
// 官方主域名、备用主域名，最后是章节页所在线路。镜像线路可能把 POST 301 成 GET，
// 因此排在最后，且总尝试数受限，避免播放首帧被无效线路拖慢。
func (d *Downloader) huangguoAIPlaybackHosts(pageURL string) []string {
	d.providerMu.Lock()
	recent := d.providerHosts[sourceHuangguoAI]
	d.providerMu.Unlock()
	candidates := []string{
		d.providerBaseURL(sourceHuangguoAI),
		recent,
		huangguoAIBaseURL,
		huangguoAIPlaybackFallbackHost,
	}
	if parsed, err := url.Parse(pageURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		candidates = append(candidates, parsed.Scheme+"://"+parsed.Host)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimRight(strings.TrimSpace(candidate), "/")
		if candidate == "" || seen[candidate] {
			continue
		}
		if !strings.HasPrefix(candidate, "http://") && !strings.HasPrefix(candidate, "https://") {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func (d *Downloader) fetchHuangguoAIPlayback(ctx context.Context, task Task, chapter Chapter) (providerMedia, error) {
	_, sourceID, valid := splitProviderDramaID(task.DramaID)
	if !valid || sourceID == "" {
		return providerMedia{}, errors.New("黄果剧集编号无效")
	}
	episode := huangguoAIEpisodeNumber(chapter, task.Index, chapter.PageURL)
	path := "/api/videos/" + url.PathEscape(sourceID) + "/episodes/" + strconv.Itoa(episode) + "/playback"
	hosts := d.huangguoAIPlaybackHosts(chapter.PageURL)
	var lastErr error
	for _, host := range hosts {
		if err := ctx.Err(); err != nil {
			return providerMedia{}, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, host+path, bytes.NewReader([]byte("{}")))
		if err != nil {
			lastErr = err
			continue
		}
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("Accept", "application/json, text/plain, */*")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Requested-With", "XMLHttpRequest")
		request.Header.Set("Origin", host)
		request.Header.Set("Referer", host+"/video/"+url.PathEscape(sourceID)+"/")
		response, err := d.doPreparedCatalogRequest(request, 10*time.Second, nil)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		closeErr := response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if closeErr != nil {
			lastErr = closeErr
			continue
		}
		if len(body) > 1<<20 {
			lastErr = errors.New("取址响应过大")
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 || catalogResponseBlockReason(response, body) != "" {
			lastErr = d.catalogResponseError(request, response, body)
			continue
		}
		media, parseErr := parseHuangguoAIPlayback(body, sourceID, episode, host)
		if parseErr == nil {
			return media, nil
		}
		lastErr = parseErr
		// 业务状态（内容下线、需要登录等）换域名也不会变，立即终止。
		if errors.Is(parseErr, errHuangguoAIPlaybackStatus) {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("无可用取址线路")
	}
	return providerMedia{}, lastErr
}

func parseHuangguoAIPlayback(body []byte, sourceID string, episode int, host string) (providerMedia, error) {
	var envelope huangguoAIPlaybackResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return providerMedia{}, fmt.Errorf("解析取址响应失败: %w", err)
	}
	if envelope.Status != 1 {
		return providerMedia{}, fmt.Errorf("%w: %s", errHuangguoAIPlaybackStatus,
			firstNonEmpty(strings.TrimSpace(envelope.Msg), "站点未授予播放权限"))
	}
	if method := envelope.Data.ManifestMethod; method != "" && !strings.EqualFold(method, "GET") {
		return providerMedia{}, fmt.Errorf("不支持的清单取回方式 %q", method)
	}
	main := strings.TrimSpace(envelope.Data.Streams.Main)
	if !isProviderHTTPMediaURL(main) {
		return providerMedia{}, errors.New("站点未返回有效播放清单")
	}
	if id := huangguoAIJSONString(envelope.Data.ID); id != "" && id != sourceID {
		return providerMedia{}, fmt.Errorf("取址响应剧集编号不匹配: %s", id)
	}
	if ep := huangguoAIJSONInt(envelope.Data.EP); ep > 0 && ep != episode {
		return providerMedia{}, fmt.Errorf("取址响应集号不匹配: %d", ep)
	}
	return providerMedia{URL: main, Referer: host + "/"}, nil
}

func huangguoAIJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}

func huangguoAIJSONInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		value, _ := strconv.Atoi(strings.TrimSpace(text))
		return value
	}
	return 0
}
