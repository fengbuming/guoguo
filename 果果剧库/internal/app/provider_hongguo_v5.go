package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beilunyang/openhongguo/adapters/memstore"
	"github.com/beilunyang/openhongguo/sdk"
	ohgmodel "github.com/beilunyang/openhongguo/sdk/model"
)

// 红果现行播放取址接口（V5，2026-11-18 抓包）：
//
//	POST /novel/player/multi_video_model/player/v1
//
// 与本项目既有的两条旧链路（multi_video_model/v1 与 video_tab/multi_video_model/v1）
// 相比有三处关键变化：端点换成 player/v1、请求体带 dr_scene="player" 且
// device_level=1、签名族换成 x-medusa/x-helios/x-khronos/x-ss-stub。旧链路
// 从 2026-11 中旬起被上游静默丢弃（HTTP 200 + 0 字节），表现为
//「红果 App 接口返回空响应」，并非该集需要登录或付费。
//
// 这里直接复用 beilunyang/openhongguo（Apache-2.0）的匿名 SDK 完成
// 设备注册、V5 签名与响应解析，网络层仍走本项目 Downloader 自己的
// catalog client（代理、限速、stall guard 等定制保持不变）。设备档案只
// 保存在内存中，进程重启后重新注册匿名设备。
//
// 参考：beilunyang/openhongguo@33c888d（internal/protocol/playback_v5.go）、
// jackotom/hongguo-drama（src/native/playback-v5.js、app-source.js）。

const hongguoV5Referer = "https://novel.snssdk.com/"

// hongguoV5Transport 把 SDK 的请求桥接到本项目既有的网络层。
type hongguoV5Transport struct {
	downloader *Downloader
}

func (t hongguoV5Transport) Do(ctx context.Context, req *sdk.Request) (*sdk.Response, error) {
	if req == nil {
		return nil, errors.New("红果 V5 请求为空")
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	for name, value := range req.Headers {
		request.Header.Set(name, value)
	}
	// SDK 依据响应头 Content-Encoding 自行解压；禁用 net/http 的透明 gzip，
	// 避免响应被解压两次或头被移除后 SDK 拿到不一致的字节。
	request.Header.Set("Accept-Encoding", "identity")
	response, err := t.downloader.doCatalogRequestWithTimeout(request, 20*time.Second)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(response.Header))
	for name, values := range response.Header {
		if len(values) > 0 {
			headers[name] = values[0]
		}
	}
	cookies := map[string]string{}
	for _, cookie := range response.Cookies() {
		if cookie.Name != "" {
			cookies[cookie.Name] = cookie.Value
		}
	}
	return &sdk.Response{
		StatusCode: response.StatusCode,
		Headers:    headers,
		Body:       body,
		Cookies:    cookies,
	}, nil
}

var (
	hongguoV5Mu      sync.Mutex
	hongguoV5Clients = make(map[*Downloader]*sdk.Client)
)

func (d *Downloader) hongguoV5Client() (*sdk.Client, error) {
	hongguoV5Mu.Lock()
	defer hongguoV5Mu.Unlock()
	if client := hongguoV5Clients[d]; client != nil {
		return client, nil
	}
	client, err := sdk.NewClient(sdk.Options{
		Transport:    hongguoV5Transport{downloader: d},
		ProfileStore: memstore.New(),
	})
	if err != nil {
		return nil, fmt.Errorf("红果 V5 客户端初始化失败：%w", err)
	}
	hongguoV5Clients[d] = client
	return client, nil
}

// resolveHongguoV5Media 匿名取得该集全部清晰度片源；对靠后集数与付费集同样有效。
func (d *Downloader) resolveHongguoV5Media(ctx context.Context, videoID string) (providerMedia, error) {
	if !hongguoNumericID.MatchString(videoID) {
		return providerMedia{}, errors.New("红果视频 ID 无效")
	}
	client, err := d.hongguoV5Client()
	if err != nil {
		return providerMedia{}, err
	}
	playback, err := client.ResolvePlayback(ctx, videoID, sdk.PlaybackOptions{})
	if playback == nil {
		if err != nil {
			return providerMedia{}, fmt.Errorf("红果 V5 取址失败：%w", err)
		}
		return providerMedia{}, errors.New("红果 V5 取址未返回播放模型")
	}
	// ErrDRMProtected 是正常返回：模型仍可用，内容密钥由 SDK 在本地派生。
	return hongguoV5MediaFromPlayback(playback)
}

// hongguoV5MediaFromPlayback 把 SDK 的播放模型映射成本项目的 providerMedia，
// 排序规则与 selectHongguoModelMedia 保持一致（H.264 优先 → 清晰度 → 体积）。
func hongguoV5MediaFromPlayback(playback *ohgmodel.Playback) (providerMedia, error) {
	var key []byte
	if playback.Protected {
		contentKey, err := sdk.ContentKey(playback)
		if err != nil {
			return providerMedia{}, fmt.Errorf("红果 V5 内容密钥不可用：%w", err)
		}
		key, err = hex.DecodeString(contentKey.Hex)
		if err != nil || len(key) != 16 {
			return providerMedia{}, errors.New("红果 V5 内容密钥格式无效")
		}
	}

	type candidate struct {
		media providerMedia
		rank  int
		pixel int
		size  int64
	}
	var choices []candidate
	for _, stream := range playback.Streams {
		address := strings.TrimSpace(stream.URL)
		if !isProviderHTTPMediaURL(address) {
			continue
		}
		rank := hongguoModelCodecRank(strings.ToLower(strings.TrimSpace(stream.Codec)))
		if rank >= 2 {
			continue
		}
		quality := stream.Height
		if digits := hongguoQualityNumber.FindString(stream.Definition); digits != "" {
			if parsed, err := strconv.Atoi(digits); err == nil && parsed > 0 {
				quality = parsed
			}
		}
		media := providerMedia{
			URL:      address,
			Referer:  hongguoV5Referer,
			Duration: time.Duration(playback.DurationSeconds) * time.Second,
			Quality:  quality,
			CENCKey:  append([]byte(nil), key...),
		}
		if backup := strings.TrimSpace(stream.BackupURL); isProviderHTTPMediaURL(backup) && backup != address {
			media.Backups = []string{backup}
		}
		if audio := strings.TrimSpace(playback.AudioURL); isProviderHTTPMediaURL(audio) {
			media.AudioURL = audio
			media.AudioCENCKey = append([]byte(nil), key...)
		}
		pixel := stream.Width * stream.Height
		if pixel <= 0 {
			pixel = quality * quality
		}
		choices = append(choices, candidate{media: media, rank: rank, pixel: pixel, size: stream.Size})
	}
	if len(choices) == 0 {
		return providerMedia{}, errors.New("红果 V5 未返回兼容的片源")
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].rank != choices[j].rank {
			return choices[i].rank < choices[j].rank
		}
		if choices[i].pixel != choices[j].pixel {
			return choices[i].pixel > choices[j].pixel
		}
		return choices[i].size > choices[j].size
	})
	selected := choices[0].media
	for _, choice := range choices {
		selected.Variants = append(selected.Variants, choice.media)
	}
	return selected, nil
}
