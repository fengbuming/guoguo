package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 红果网页播放器（hongguoduanju.com）的受保护取流接口：
// 匿名即可调用，但仅对免费试看集返回媒体，且清晰度上限 720p。
// 视频/音频为分离的 DASH 轨道（CENC 加密），player_access_key 经
// hongguoContentKey（tag "22" 加法变体）派生 AES-128 密钥。

const hongguoProtectedPath = "/api/video/protected/play-info"

// errHongguoProtectedLocked 表示该集需要登录或付费（匿名仅试看集可播放），
// 收到此错误时无需再尝试其他取流路径。
var errHongguoProtectedLocked = errors.New("红果该集需要登录或付费，匿名仅试看集可播放")

type hongguoProtectedTrack struct {
	MainURL         string `json:"main_url"`
	BackupURL       string `json:"backup_url"`
	PlayerAccessKey string `json:"player_access_key"`
	Kid             string `json:"kid"`
}

type hongguoProtectedPlayInfo struct {
	Vid             string `json:"vid"`
	Kid             string `json:"kid"`
	MainURL         string `json:"main_url"`
	BackupURL       string `json:"backup_url"`
	PlayerAccessKey string `json:"player_access_key"`
	Format          string `json:"format"`
	CodecType       string `json:"codec_type"`
	Width           string `json:"width"`
	Height          string `json:"height"`
	Definition      string `json:"definition"`
	Duration        float64 `json:"duration"`
	Audio           hongguoProtectedTrack `json:"audio"`
}

type hongguoProtectedResponse struct {
	PlayInfo *hongguoProtectedPlayInfo `json:"play_info"`
	Error    string                    `json:"error"`
}

func (d *Downloader) resolveHongguoProtectedMedia(ctx context.Context, seriesID, videoID string) (providerMedia, error) {
	if !hongguoNumericID.MatchString(seriesID) || !hongguoNumericID.MatchString(videoID) {
		return providerMedia{}, errors.New("红果播放请求缺少有效的剧集 ID")
	}
	payload, err := json.Marshal(map[string]any{"series_id": seriesID, "item_id": videoID, "definition": "720p"})
	if err != nil {
		return providerMedia{}, err
	}
	body, err := d.fetchHongguoProtectedText(ctx, payload, hongguoBaseURL+"/player/"+seriesID+"/"+videoID)
	if err != nil {
		return providerMedia{}, err
	}
	return hongguoProtectedMediaFromResponse([]byte(body))
}

// hongguoProtectedMediaFromResponse 解析 play-info 响应并构造双轨 DASH 媒体。
func hongguoProtectedMediaFromResponse(body []byte) (providerMedia, error) {
	var response hongguoProtectedResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return providerMedia{}, errors.New("红果受保护播放接口返回了无效数据")
	}
	info := response.PlayInfo
	if info == nil || !isProviderHTTPMediaURL(strings.TrimSpace(info.MainURL)) {
		if strings.TrimSpace(response.Error) == "protected_playback_unavailable" {
			return providerMedia{}, errHongguoProtectedLocked
		}
		if message := strings.TrimSpace(response.Error); message != "" {
			return providerMedia{}, fmt.Errorf("红果受保护播放不可用：%s", message)
		}
		return providerMedia{}, errors.New("红果受保护播放接口未返回可用的媒体地址")
	}
	codec := strings.ToLower(strings.TrimSpace(info.CodecType))
	if codec != "" && codec != "h264" && codec != "avc1" && codec != "avc" {
		return providerMedia{}, fmt.Errorf("红果该集编码暂不支持（%s）", strings.TrimSpace(info.CodecType))
	}
	key, err := hongguoContentKey(info.PlayerAccessKey)
	if err != nil {
		return providerMedia{}, fmt.Errorf("红果受保护播放密钥解析失败：%w", err)
	}
	media := providerMedia{
		URL:      strings.TrimSpace(info.MainURL),
		Referer:  hongguoBaseURL + "/",
		Duration: time.Duration(info.Duration * float64(time.Second)),
		CENCKey:  key,
		Quality:  720,
	}
	if definition, err := strconv.Atoi(hongguoQualityNumber.FindString(info.Definition)); err == nil && definition > 0 {
		media.Quality = definition
	}
	if audioURL := strings.TrimSpace(info.Audio.MainURL); isProviderHTTPMediaURL(audioURL) {
		media.AudioURL = audioURL
		audioKey := key
		if access := strings.TrimSpace(info.Audio.PlayerAccessKey); access != "" && access != strings.TrimSpace(info.PlayerAccessKey) {
			audioKey, err = hongguoContentKey(access)
			if err != nil {
				return providerMedia{}, fmt.Errorf("红果受保护播放音轨密钥解析失败：%w", err)
			}
		}
		media.AudioCENCKey = audioKey
	}
	// 备用 CDN 节点作为根资源回退（完整拷贝，避免清晰度切换时丢失密钥与音轨）。
	if backup := strings.TrimSpace(info.BackupURL); isProviderHTTPMediaURL(backup) && backup != media.URL {
		variant := media
		variant.URL = backup
		variant.Variants = nil
		media.Variants = []providerMedia{variant}
	}
	return media, nil
}

// fetchHongguoProtectedText 以浏览器身份 POST 红果网页接口（无需签名）。
func (d *Downloader) fetchHongguoProtectedText(ctx context.Context, payload []byte, referer string) (string, error) {
	if referer == "" {
		referer = hongguoBaseURL + "/"
	}
	attempts := d.cfg.Retries
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 3 {
		attempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, hongguoBaseURL+hongguoProtectedPath, bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Referer", referer)
		request.Header.Set("Origin", hongguoBaseURL)
		request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		response, err := d.doCatalogRequestWithTimeout(request, providerTimeout)
		if err != nil {
			lastErr = err
			var backoff *requestBackoff
			if errors.As(err, &backoff) || ctx.Err() != nil {
				return "", err
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
		closeErr := response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if closeErr != nil {
			lastErr = closeErr
			continue
		}
		if len(body) > providerMaxBodyBytes {
			lastErr = fmt.Errorf("response exceeds %d bytes", providerMaxBodyBytes)
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			// 站点用 4xx + JSON 错误体表达“该集需要登录或付费”，交给解析层给出明确原因，不再重试。
			if isHongguoProtectedErrorStatus(response.StatusCode) && json.Valid(body) && bytes.Contains(body, []byte(`"error"`)) {
				return string(body), nil
			}
			lastErr = fmt.Errorf("红果受保护播放接口 HTTP %d", response.StatusCode)
			continue
		}
		return string(body), nil
	}
	return "", lastErr
}

func isHongguoProtectedErrorStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return status >= 400 && status < 500
}

func dramaSource(drama Drama) string {
	if source := strings.TrimSpace(drama.Source); source != "" {
		return source
	}
	if source, _, ok := splitProviderDramaID(drama.ID); ok {
		return source
	}
	return ""
}
