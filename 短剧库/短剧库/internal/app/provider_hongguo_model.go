package app

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 红果 App 取址链路（multi_video_model）。
//
// 与老路 video_model/v1 的区别：
//  1. 请求体是 mixed_video_id_map（不是 video_id/content_type）；
//  2. 需要六个签名头（见 provider_hongguo_model_sign.go）；
//  3. 接口只回 fallback_api，真正的片源列表要再 GET 一次 fallback；
//  4. 片源 URL 可能被 key_seed 加密，需 AES-128-CBC 解密。
//
// 这条链路匿名可用，且能取到全部集（含付费集）与 360p~1080p 全部清晰度。

const hongguoModelPath = "/novel/player/multi_video_model/v1/"
const hongguoModelUserAgent = "com.phoenix.read/71332 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3; Cronet/TTNetVersion:04657795 2026-01-23 QuicVersion:c67e9834 2025-09-08)"
const hongguoModelVersionName = "7.1.3.32"

// hongguoURLConstants: app-source.js 的 64 字节常量段，用于派生 URL 解密密钥。
var hongguoURLConstants = []byte{
	0x4d, 0xd4, 0xc2, 0xe6, 0xb8, 0x31, 0x62, 0x09, 0x0e, 0x52, 0xb3, 0xc7, 0xa6, 0x73, 0x3b, 0xa4,
	0x1c, 0xb2, 0x46, 0x2b, 0x82, 0x9a, 0xb5, 0x8a, 0x19, 0x6b, 0x39, 0xdb, 0x57, 0x17, 0x75, 0x24,
	0xf4, 0x9b, 0xaf, 0x7f, 0x08, 0xe8, 0xd6, 0x8d, 0x26, 0xa7, 0x2e, 0x37, 0xc1, 0xa9, 0x5a, 0x2f,
	0x1f, 0x05, 0xa5, 0x18, 0x92, 0xae, 0xf2, 0x94, 0x97, 0x32, 0xb6, 0x2a, 0x38, 0xaa, 0xdd, 0x58,
}

func hongguoModelQueryPairs(iid, deviceID, khronos, ticket string) [][2]string {
	return [][2]string{
		{"ac", "wifi"}, {"channel", "update_64"}, {"aid", "8662"},
		{"app_name", "novelread"}, {"version_code", "71332"},
		{"version_name", hongguoModelVersionName},
		{"device_platform", "android"}, {"os", "android"}, {"ssmix", "a"},
		{"device_type", "25053RT47C"}, {"device_brand", "Redmi"}, {"language", "zh"},
		{"os_api", "36"}, {"os_version", "16"}, {"manifest_version_code", "71332"},
		{"resolution", "1280*2772"}, {"dpi", "520"}, {"update_version_code", "71332"},
		{"host_abi", "arm64-v8a"}, {"dragon_device_type", "phone"}, {"pv_player", "71332"},
		{"compliance_status", "0"}, {"need_personal_recommend", "1"},
		{"player_so_load", "1"}, {"is_android_pad_screen", "0"},
		{"iid", iid}, {"device_id", deviceID},
		{"ts", khronos}, {"_rticket", ticket},
	}
}

// resolveHongguoModelMedia 走 multi_video_model 两阶段取址，返回可解密的媒体。
func (downloader *Downloader) resolveHongguoModelMedia(ctx context.Context, videoID string) (providerMedia, error) {
	if !hongguoNumericID.MatchString(videoID) {
		return providerMedia{}, errors.New("红果视频 ID 无效")
	}
	result, err := downloader.postHongguoModelV2(ctx, videoID)
	if err != nil && !errors.Is(err, errHongguoPlayerEmpty) {
		// 新签名链路遇到网络/HTTP 异常时保留旧链路作为兼容回退；空响应是上游
		// 静默丢弃，旧签名同样拿不到数据，不再重复请求以免拖慢播放解析。
		result, err = downloader.postHongguoModel(ctx, videoID)
	}
	if err != nil {
		return providerMedia{}, err
	}
	if code := firstNonEmpty(mapString(result, "code", "Code")); code != "" && code != "0" {
		return providerMedia{}, fmt.Errorf("红果取址接口暂不可用（%s）", truncate(code, 20))
	}
	entry := nestedMap(nestedMap(result, "data"), videoID)
	if len(entry) == 0 {
		return providerMedia{}, errors.New("红果取址接口未返回所选分集")
	}
	model := hongguoModelAsObject(entry["video_model"])
	if len(model) == 0 {
		return providerMedia{}, errors.New("红果取址接口未返回片源入口")
	}
	fallback := hongguoModelFallback(model["fallback_api"])
	if fallback == "" || !isProviderHTTPMediaURL(fallback) {
		return providerMedia{}, errors.New("红果取址接口未返回片源地址")
	}
	outer, err := downloader.getHongguoModelPlain(ctx, fallback)
	if err != nil {
		return providerMedia{}, err
	}
	if code := firstNonEmpty(mapString(outer, "code", "Code"), mapString(nestedMap(outer, "video_info"), "code", "Code")); code != "" && code != "0" {
		return providerMedia{}, fmt.Errorf("红果片源接口暂不可用（%s）", truncate(code, 20))
	}
	info := nestedMap(nestedMap(outer, "video_info"), "data")
	if len(info) == 0 {
		return providerMedia{}, errors.New("红果片源接口未返回片源模型")
	}
	var seed []byte
	if encoded := mapString(info, "key_seed"); encoded != "" {
		seed, _ = base64.StdEncoding.DecodeString(encoded)
	}
	return selectHongguoModelMedia(nestedMap(info, "video_list"), seed)
}

func (downloader *Downloader) postHongguoModel(ctx context.Context, videoID string) (map[string]any, error) {
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
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	client := downloader.hongguoClient()
	client.mu.Lock()
	deviceID, installID := client.state.DeviceID, client.state.InstallID
	client.mu.Unlock()

	khronos := time.Now().Unix()
	query := encodeHongguoModelQuery(hongguoModelQueryPairs(installID, deviceID,
		strconv.FormatInt(khronos, 10), strconv.FormatInt(time.Now().UnixMilli(), 10)))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		hongguoAppBaseURL+hongguoModelPath+"?"+query, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	signature := signHongguoModel(query, body, khronos)
	bodyHash := fmt.Sprintf("%X", md5.Sum(body))
	request.Header.Set("User-Agent", hongguoModelUserAgent)
	request.Header.Set("Accept", "application/json; charset=utf-8,application/x-protobuf")
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	request.Header.Set("x-xs-from-web", "0")
	request.Header.Set("x-ss-req-ticket", strconv.FormatInt(time.Now().UnixMilli(), 10))
	request.Header.Set("x-tt-request-tag", "t=0;n=0")
	request.Header.Set("sdk-version", "2")
	request.Header.Set("passport-sdk-version", "50561")
	request.Header.Set("x-vc-bdturing-sdk-version", "3.7.2.cn")
	request.Header.Set("x-ss-stub", bodyHash)
	request.Header.Set("x-gorgon", signature.gorgon)
	request.Header.Set("x-khronos", signature.khronos)
	request.Header.Set("x-ladon", signature.ladon)
	request.Header.Set("x-argus", signature.argus)
	request.Header.Set("x-helios", signature.helios)
	request.Header.Set("x-tt-dt", "")

	response, err := downloader.doPreparedCatalogRequest(request, 20*time.Second, nil)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, &httpStatusError{host: strings.ToLower(request.URL.Hostname()),
			status: response.StatusCode, reason: "红果取址接口暂不可用"}
	}
	if len(content) == 0 || len(content) > providerMaxBodyBytes {
		return nil, errors.New("红果取址接口未返回有效数据")
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, errors.New("红果取址接口返回格式异常")
	}
	return result, nil
}

// getHongguoModelPlain: fallback_api 的二次请求，不带签名，只带 UA。
func (downloader *Downloader) getHongguoModelPlain(ctx context.Context, target string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", hongguoModelUserAgent)
	response, err := downloader.doPreparedCatalogRequest(request, 20*time.Second, nil)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, &httpStatusError{host: strings.ToLower(request.URL.Hostname()),
			status: response.StatusCode, reason: "红果片源接口暂不可用"}
	}
	if len(content) == 0 || len(content) > providerMaxBodyBytes {
		return nil, errors.New("红果片源接口未返回有效数据")
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, errors.New("红果片源接口返回格式异常")
	}
	return result, nil
}

func hongguoModelAsObject(value any) map[string]any {
	if text, ok := value.(string); ok {
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var parsed map[string]any
		if decoder.Decode(&parsed) == nil {
			return parsed
		}
		return nil
	}
	return nestedMap(value)
}

// hongguoModelFallback: fallback_api 可能是字符串、数组或嵌套对象。
func hongguoModelFallback(value any) string {
	switch typed := value.(type) {
	case string:
		return hongguoCleanURL(typed)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				return hongguoCleanURL(text)
			}
			if nested := hongguoModelFallback(item); nested != "" {
				return nested
			}
		}
	case map[string]any:
		return hongguoModelFallback(typed["fallback_api"])
	}
	return ""
}

func hongguoCleanURL(value string) string {
	return strings.NewReplacer(`\/`, "/", `\u0026`, "&", "&amp;", "&").Replace(strings.TrimSpace(value))
}

// hongguoDecryptSpadeURL: seed 双 sha512 派生 AES-128-CBC 密钥/IV 解密媒体 URL。
func hongguoDecryptSpadeURL(value string, seed []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	if len(raw) < 5 {
		return "", errors.New("红果片源地址密文过短")
	}
	if raw[0] != 0xa8 || raw[2] != 0x01 || raw[3] != 0x00 {
		return "", errors.New("红果片源地址格式错误")
	}
	length := ((len(raw) - 4) / 16) * 16
	if length == 0 {
		return "", errors.New("红果片源地址无完整块")
	}
	first := sha512.Sum512(seed)
	second := sha512.Sum512(append(append([]byte(nil), first[:]...), hongguoURLConstants...))
	block, err := aes.NewCipher(second[:16])
	if err != nil {
		return "", err
	}
	plain := make([]byte, length)
	cipher.NewCBCDecrypter(block, second[16:32]).CryptBlocks(plain, raw[4:4+length])
	end := len(plain)
	if end > 0 {
		padding := int(plain[end-1])
		if padding >= 1 && padding <= 16 && padding <= end {
			end -= padding
		}
	}
	for end > 0 && plain[end-1] == 0 {
		end--
	}
	address := hongguoCleanURL(string(plain[:end]))
	if !isProviderHTTPMediaURL(address) {
		return "", errors.New("红果片源地址解密结果无效")
	}
	return address, nil
}

// selectHongguoModelMedia 在全部清晰度候选中择优：H.264 优先，其次清晰度，最后体积。
func selectHongguoModelMedia(list map[string]any, seed []byte) (providerMedia, error) {
	type candidate struct {
		media providerMedia
		rank  int
		pixel int
		size  int64
	}
	var choices []candidate
	var keyErr error
	keys := make([]string, 0, len(list))
	for key := range list {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		row, _ := list[key].(map[string]any)
		if len(row) == 0 {
			continue
		}
		codec := strings.ToLower(firstNonEmpty(mapString(row, "codec_type"),
			mapString(nestedMap(row, "video_meta"), "codec_type")))
		rank := hongguoModelCodecRank(codec)
		if rank >= 2 {
			continue
		}
		address, backups := hongguoModelMediaAddresses(row, seed)
		if address == "" {
			continue
		}
		media := providerMedia{URL: address, Backups: backups, Referer: "https://novel.snssdk.com/"}
		if duration, err := strconv.ParseFloat(firstNonEmpty(
			mapString(row, "duration"), mapString(nestedMap(row, "video_meta"), "duration")), 64); err == nil && duration > 0 {
			media.Duration = time.Duration(duration * float64(time.Second))
		}
		spade := firstNonEmpty(mapString(row, "spade_a"),
			mapString(nestedMap(row, "encrypt_info"), "spade_a"))
		if spade != "" || row["encrypt"] == true ||
			mapString(row, "encryption_method") == "cenc-aes-ctr" {
			// 取址链路下发的是 App 版（tag="11"）密钥，走减法派生。
			derived, err := hongguoContentKey(spade)
			if err != nil {
				keyErr = err
				continue
			}
			media.CENCKey = derived
		}
		width, height := hongguoModelDimensions(row)
		media.Quality = height
		pixel := width * height
		if definition := hongguoQualityNumber.FindString(mapString(row, "definition")); definition != "" {
			if value, err := strconv.Atoi(definition); err == nil && value > 0 {
				media.Quality = value
				pixel = value * value * 2 // 竖屏：短边 * 长边近似
			}
		}
		size := int64(0)
		if parsed, err := strconv.ParseInt(firstNonEmpty(mapString(row, "size"),
			mapString(nestedMap(row, "video_meta"), "size")), 10, 64); err == nil {
			size = parsed
		}
		choices = append(choices, candidate{media: media, rank: rank, pixel: pixel, size: size})
	}
	if len(choices) == 0 {
		if keyErr != nil {
			return providerMedia{}, fmt.Errorf("红果片源密钥不可用: %w", keyErr)
		}
		return providerMedia{}, errors.New("红果未返回兼容的片源")
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

func hongguoModelCodecRank(codec string) int {
	switch codec {
	case "h264", "avc", "avc1":
		return 0
	case "hevc", "h265", "hvc1", "hev1", "bytevc1":
		return 1
	}
	return 2
}

func hongguoModelDimensions(row map[string]any) (int, int) {
	groups := [][]string{
		{"vwidth", "vheight"}, {"width", "height"},
	}
	for _, group := range groups {
		sources := []map[string]any{row, nestedMap(row, "video_meta")}
		for _, source := range sources {
			width, errW := strconv.Atoi(mapString(source, group[0]))
			height, errH := strconv.Atoi(mapString(source, group[1]))
			if errW == nil && errH == nil && width > 0 && height > 0 {
				return width, height
			}
		}
	}
	return 0, 0
}

// hongguoModelMediaAddress 依次尝试各地址字段，命中 key_seed 时解密。
func hongguoModelMediaAddress(row map[string]any, seed []byte) string {
	main, _ := hongguoModelMediaAddresses(row, seed)
	return main
}

// hongguoModelMediaAddresses 收集该清晰度的全部地址：首个为主地址，其余为备用节点。
// 同一清晰度的 main_url 与 backup_url_* 指向同一份视频的不同 CDN 节点。
func hongguoModelMediaAddresses(row map[string]any, seed []byte) (string, []string) {
	var main string
	var backups []string
	seen := map[string]bool{}
	for _, field := range []string{"main_url", "backup_url_1", "backup_url_2", "backup_url"} {
		value := strings.TrimSpace(mapString(row, field))
		if value == "" {
			continue
		}
		if !isProviderHTTPMediaURL(value) {
			if len(seed) == 0 {
				continue
			}
			decrypted, err := hongguoDecryptSpadeURL(value, seed)
			if err != nil {
				continue
			}
			value = decrypted
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		if main == "" {
			main = value
		} else {
			backups = append(backups, value)
		}
	}
	return main, backups
}
