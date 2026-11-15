package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 红果 App 接口签名升级（2026-11）。
//
// 上游网关已不再接受旧的六头签名（x-gorgon RC4 + 4 字节 ladon/argus + helios，
// 见 provider_hongguo_model_sign.go）与老版 X-Gorgon（provider_hongguo_sign.go）：
// 这些请求一律被静默丢弃，表现为 HTTP 200 + 0 字节正文。当前有效的是 551 版
// Argus/Ladon 签名（provider_hongguo_player_sign.go）配合 7.4.3.32 设备档案。
//
// 参考 2026-11 仍活跃、高 star 的公开实现：jackotom/hongguo-drama（GPL-3.0-only）、
// zfdang/hongguo-open-player，以及同源 Go 实现 ZhenGuoJian-Web。
// 实测：同一网关下 video_detail/v1 用新签名稳定返回 ~66KB，旧签名返回 0 字节。
//
// 本文件只新增请求构造层；片源解析、密钥派生、清晰度择优、备用节点、
// 播放与缓存链路全部沿用原有实现，未做任何改动。

const (
	hongguoPlayerVersionCode = "74332"
	hongguoPlayerVersionName = "7.4.3.32"
	hongguoPlayerUserAgent   = "com.phoenix.read/74332 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3; Cronet/TTNetVersion:04657795 2026-01-23 QuicVersion:c67e9834 2025-09-08)"

	hongguoPlayerHostPrimary = "https://api5-normal-sinfonlineb.fqnovel.com"
	hongguoPlayerHostBackup  = "https://api5-normal-sinfonlinea.fqnovel.com"
)

// errHongguoPlayerEmpty 表示上游静默丢弃请求（HTTP 200 + 0 字节）。
// 这类失败换回旧签名也必然失败，因此不应再触发旧链路回退。
var errHongguoPlayerEmpty = errors.New("红果 App 接口返回空响应")

// hongguoPlayerCDID 生成一次性设备指纹，格式与官方 device_register 一致。
func hongguoPlayerCDID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "2b1c9d3e-6f40-4a51-8c92-5e7a1d0b3f44"
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// hongguoPlayerEncode 与 python urllib.parse.quote 对齐：保留 *，其余非安全字符转 %XX。
func hongguoPlayerEncode(value string) string {
	const digits = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' ||
			char == '.' || char == '~' || char == '*' {
			builder.WriteByte(char)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(digits[char>>4])
		builder.WriteByte(digits[char&15])
	}
	return builder.String()
}

// hongguoPlayerQuery 构造 7.4.3.32 设备档案的完整 query；签名按这一串字节计算。
func hongguoPlayerQuery(installID, deviceID, cdid string, extra [][2]string, now time.Time) string {
	pairs := [][2]string{
		{"ac", "wifi"}, {"channel", "update_64"}, {"aid", "8662"},
		{"app_name", "novelread"}, {"version_code", hongguoPlayerVersionCode},
		{"version_name", hongguoPlayerVersionName},
		{"device_platform", "android"}, {"os", "android"}, {"ssmix", "a"},
		{"device_type", "25053RT47C"}, {"device_brand", "Redmi"}, {"language", "zh"},
		{"os_api", "36"}, {"os_version", "16"},
		{"manifest_version_code", hongguoPlayerVersionCode},
		{"resolution", "1280*2772"}, {"dpi", "520"},
		{"update_version_code", hongguoPlayerVersionCode},
		{"host_abi", "arm64-v8a"}, {"dragon_device_type", "phone"},
		{"pv_player", hongguoPlayerVersionCode},
		{"compliance_status", "0"}, {"need_personal_recommend", "1"},
		{"player_so_load", "1"}, {"is_android_pad_screen", "0"},
		{"iid", installID}, {"device_id", deviceID}, {"cdid", cdid},
	}
	pairs = append(pairs, extra...)
	pairs = append(pairs,
		[2]string{"ts", strconv.FormatInt(now.Unix(), 10)},
		[2]string{"_rticket", strconv.FormatInt(now.UnixMilli(), 10)})
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, hongguoPlayerEncode(pair[0])+"="+hongguoPlayerEncode(pair[1]))
	}
	return strings.Join(parts, "&")
}

// hongguoPlayerSignedJSON 用 551 签名发一次 App 请求，返回解析后的 JSON。
// 请求体保持原始 JSON（不压缩）：实测压缩与非压缩均被接受，保持原样可避免
// 影响既有调用方对请求体的读取。
func (downloader *Downloader) hongguoPlayerSignedJSON(ctx context.Context, host, path string,
	extra [][2]string, payload any, timeout time.Duration) (map[string]any, error) {
	client := downloader.hongguoClient()
	client.mu.Lock()
	deviceID, installID := client.state.DeviceID, client.state.InstallID
	client.mu.Unlock()

	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = encoded
	}
	cdid := hongguoPlayerCDID()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, host+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", hongguoPlayerUserAgent)
	request.Header.Set("Accept", "application/json; charset=utf-8,application/x-protobuf")
	request.Header.Set("sdk-version", "2")
	request.Header.Set("x-xs-from-web", "0")
	request.Header.Set("passport-sdk-version", "5051452")
	request.Header.Set("x-ss-dp", "8662")
	request.Header.Set("x-tt-dt", "")
	request.Header.Set("x-tt-store-region", "cn")
	request.Header.Set("x-tt-store-region-src", "did")
	if body != nil {
		request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	}

	response, err := downloader.doPreparedCatalogRequest(request, timeout, func(prepared *http.Request) {
		now := time.Now()
		prepared.URL.RawQuery = hongguoPlayerQuery(installID, deviceID, cdid, extra, now)
		signHongguoPlayerRequest(prepared, body, now)
	})
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	_ = response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, &httpStatusError{
			host:   strings.ToLower(request.URL.Hostname()),
			status: response.StatusCode, reason: "红果 App 接口暂不可用",
		}
	}
	if len(content) == 0 || len(content) > providerMaxBodyBytes {
		return nil, errHongguoPlayerEmpty
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, errors.New("红果 App 接口返回格式异常")
	}
	return result, nil
}

// hongguoPlayerRequest 在主/备域名间有限重试；明确的 4xx 不做无谓重试。
func (downloader *Downloader) hongguoPlayerRequest(ctx context.Context, path string,
	extra [][2]string, payload any, attempts int, timeout time.Duration) (map[string]any, error) {
	if attempts < 1 {
		attempts = 1
	}
	hosts := []string{hongguoPlayerHostPrimary, hongguoPlayerHostBackup}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := downloader.hongguoPlayerSignedJSON(ctx, hosts[attempt%len(hosts)],
			path, extra, payload, timeout)
		if err == nil {
			return result, nil
		}
		lastErr = err
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) && statusErr.status >= 400 && statusErr.status < 500 {
			break
		}
	}
	return nil, lastErr
}

// postHongguoModelV2 是取址链路的新签名入口；响应结构与旧接口一致，
// 后续的 fallback_api 二次请求、key_seed 解密、spade_a 派生与清晰度择优全部复用原实现。
func (downloader *Downloader) postHongguoModelV2(ctx context.Context, videoID string) (map[string]any, error) {
	payload := map[string]any{
		"biz_param": map[string]any{
			"detail_page_version":       0,
			"device_level":              3,
			"disable_digg_stat":         false,
			"need_all_video_definition": true,
			"need_mp4_align":            false,
			"use_os_player":             false,
			"use_server_dns":            false,
			"video_platform":            1024,
		},
		"mixed_video_id_map": map[string]any{"1004": []string{videoID}},
	}
	return downloader.hongguoPlayerRequest(ctx, hongguoModelPath, nil, payload, 2, 10*time.Second)
}
