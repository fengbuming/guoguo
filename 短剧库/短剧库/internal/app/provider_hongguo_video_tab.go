package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 红果 App 现行取址链路（2026-11）。
//
// 上游网关已停发旧 POST /novel/player/multi_video_model/v1/ 的数据（HTTP 200 +
// 0 字节），并且从 2026-11 起要求「一次性注册的设备身份」：同样的 551 签名，
// 随机 device_id/iid/cdid 一律返回空响应，只有先调用 device_register 拿到
// 真实 device_id/install_id 才能取到片源。实测对照：
//
//	随机身份 + video_tab GET      -> 200 / 0 字节
//	注册身份 + video_tab GET      -> 200 / ~22KB，含 inline video_list 与 fallback_api
//
// 现行顺序为：device_register -> GET /reading/bookapi/video_tab/
// multi_video_model/v1/（7.3.9.32 设备档案 + 551 Argus/Ladon 签名）。上游对
// 匿名取址还有概率性静默丢弃，因此按参考实现做 pacing + 全新身份重试，并在
// 同一身份下轮询多个集群的 /novel/player/multi_video_model/player/v1，最后落到
// 番茄短剧通道（/novel/player/video_model/v1/）。
// 参考 2026-11 仍活跃的公开实现 jackotom/hongguo-drama（GPL-3.0-only）、
// zfdang/hongguo-open-player（GPL-3.0-only）。
//
// 本文件只负责「注册身份 + 取回模型」。模型的片源解析、key_seed 解密、
// spade_a 派生、H.264 优先与清晰度/体积择优、备用 CDN 节点，以及播放、
// remux、缓存与内存优化全部沿用 provider_hongguo_model.go 的既有实现。

const (
	hongguoVideoTabPath        = "/reading/bookapi/video_tab/multi_video_model/v1/"
	hongguoVideoTabVersionCode = "73932"
	hongguoVideoTabVersionName = "7.3.9.32"
	hongguoVideoTabUserAgent   = "com.phoenix.read/73932 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3; Cronet/TTNetVersion:04657795 2026-01-23 QuicVersion:c67e9834 2025-09-08)"

	// hongguoModelPlayerPath 是现行取址路径（比旧的 /v1/ 多一段 player）。
	hongguoModelPlayerPath = "/novel/player/multi_video_model/player/v1"
	// hongguoShortplayPath 是番茄短剧通道，UA 与 aid 与红果不同。
	hongguoShortplayPath      = "/novel/player/video_model/v1/"
	hongguoShortplayUserAgent = "com.dragon.read"

	hongguoRegisterUserAgent = "com.phoenix.read/72232 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3)"
	hongguoDeviceRegisterURL = "https://log5-applog-lf.fqnovel.com/service/2/device_register/?tt_data=a&cronet_version=8d40f833_2026-03-03&ttnet_version=4.2.243.31-douyin&use_store_region_cookie=1"
)

// hongguoPlaybackIdentity 是一次性匿名播放设备身份，签名 query 与注册请求绑定。
type hongguoPlaybackIdentity struct {
	DeviceID  string
	InstallID string
	CDID      string
}

// hongguoRegisterPad 补齐 PKCS#7 填充；专用版没有 pkcs7Pad，这里独立命名避免冲突。
func hongguoRegisterPad(src []byte, blockSize int) []byte {
	padding := blockSize - len(src)%blockSize
	out := append([]byte(nil), src...)
	return append(out, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

// registerHongguoPlaybackDevice 走 applog device_register 换取一次性的
// device_id / install_id。请求体是 gzip 后的设备档案，经 sha512 派生
// AES-128-CBC 密钥加密（ttEncrypt），并把整体 md5 放进 x-ss-stub。
func (downloader *Downloader) registerHongguoPlaybackDevice(ctx context.Context) (hongguoPlaybackIdentity, error) {
	now := time.Now()
	cdid := hongguoPlayerCDID()
	openudid := make([]byte, 8)
	if _, err := rand.Read(openudid); err != nil {
		return hongguoPlaybackIdentity{}, err
	}
	header := map[string]any{
		"display_name": "红果免费短剧", "update_version_code": 73932, "manifest_version_code": 73932,
		"app_version_minor": "7.3.9.32", "aid": 8662, "channel": "update_64", "package": "com.phoenix.read",
		"app_version": "7.3.9.32", "version_code": 73932, "sdk_version": "3.7.0-rc.34-fanqie-xiaoshuo",
		"sdk_target_version": 29, "git_hash": "3a88f4e", "os": "Android", "os_version": "16", "os_api": 36,
		"device_model": "25053RT47C", "device_brand": "Redmi", "device_manufacturer": "Redmi",
		"cpu_abi": "arm64-v8a", "release_build": "b5e187f", "density_dpi": 520, "display_density": "mdpi",
		"resolution": "1280x2772", "language": "zh", "timezone": 8, "access": "wifi", "not_request_sender": 0,
		"carrier": "CHINA MOBILE", "mcc_mnc": "46000", "rom": "eng.root.20231116.160340",
		"rom_version": "W528JS release-keys", "cdid": cdid,
		"sig_hash": "56a962410c494bbaf0b58dba20cae56f", "openudid": hex.EncodeToString(openudid),
		"clientudid": hongguoPlayerCDID(), "ipv6_list": []any{}, "region": "CN", "tz_name": "Asia/Shanghai",
		"tz_offset": 28800, "sim_region": "cn", "sim_serial_number": []any{}, "oaid_may_support": false,
		"req_id": hongguoPlayerCDID(), "device_platform": "android",
		"custom": map[string]any{
			"account_region_src": "fallback", "account_region": "cn",
			"host_bit": 64, "dragon_device_type": "phone",
		},
		"pre_installed_channel": "", "apk_first_install_time": now.UnixMilli(),
		"is_system_app": 0, "sdk_flavor": "china", "guest_mode": 0,
	}
	document, err := json.Marshal(map[string]any{
		"magic_tag": "ss_app_log", "header": header, "_gen_time": now.UnixMilli(),
	})
	if err != nil {
		return hongguoPlaybackIdentity{}, err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(document); err != nil {
		writer.Close()
		return hongguoPlaybackIdentity{}, err
	}
	if err := writer.Close(); err != nil {
		return hongguoPlaybackIdentity{}, err
	}

	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return hongguoPlaybackIdentity{}, err
	}
	inner := sha512.Sum512(random)
	derived := sha512.Sum512(append(append([]byte(nil), inner[:]...), hongguoURLConstants...))
	digest := sha512.Sum512(compressed.Bytes())
	plaintext := append(append([]byte(nil), digest[:]...), compressed.Bytes()...)
	block, err := aes.NewCipher(derived[:16])
	if err != nil {
		return hongguoPlaybackIdentity{}, err
	}
	padded := hongguoRegisterPad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, derived[16:32]).CryptBlocks(ciphertext, padded)
	data := append([]byte{0x74, 0x63, 0x05, 0x10, 0x00, 0x00}, random...)
	data = append(data, ciphertext...)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, hongguoDeviceRegisterURL, bytes.NewReader(data))
	if err != nil {
		return hongguoPlaybackIdentity{}, err
	}
	request.Header.Set("User-Agent", hongguoRegisterUserAgent)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/octet-stream;tt-data=a")
	request.Header.Set("log-encode-type", "gzip")
	request.Header.Set("x-ss-dp", "8662")
	request.Header.Set("x-ss-req-ticket", strconv.FormatInt(now.UnixMilli(), 10))
	request.Header.Set("x-vc-bdturing-sdk-version", "4.0.3.cn")
	request.Header.Set("lc", "101")
	request.Header.Set("sdk-version", "2")
	request.Header.Set("passport-sdk-version", "5051452")
	request.Header.Set("x-neptune", "-8|50:51:59:30:40:47:49:39")
	stub := md5.Sum(data)
	request.Header.Set("x-ss-stub", fmt.Sprintf("%X", stub))

	response, err := downloader.doCatalogRequestWithTimeout(request, 8*time.Second)
	if err != nil {
		return hongguoPlaybackIdentity{}, err
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	response.Body.Close()
	if readErr != nil {
		return hongguoPlaybackIdentity{}, readErr
	}
	if response.StatusCode != http.StatusOK || len(content) == 0 {
		return hongguoPlaybackIdentity{}, errors.New("红果播放设备注册失败")
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil || result == nil {
		return hongguoPlaybackIdentity{}, errors.New("红果播放设备注册响应无效")
	}
	if code := firstNonEmpty(mapString(result, "code", "Code")); code != "" && code != "0" {
		return hongguoPlaybackIdentity{}, fmt.Errorf("红果播放设备注册被拒绝（%s）", truncate(code, 20))
	}
	deviceID := firstNonEmpty(mapString(result, "device_id_str"), mapString(result, "device_id"))
	installID := firstNonEmpty(mapString(result, "install_id_str"), mapString(result, "install_id"))
	if !hongguoNumericID.MatchString(deviceID) || !hongguoNumericID.MatchString(installID) {
		return hongguoPlaybackIdentity{}, errors.New("红果播放设备注册未返回有效身份")
	}
	return hongguoPlaybackIdentity{DeviceID: deviceID, InstallID: installID, CDID: cdid}, nil
}

// hongguoVideoTabQuery 构造 7.3.9.32 设备档案的 video_tab query；
// 字段顺序与签名计算字节级绑定，不可随意调整。
func hongguoVideoTabQuery(identity hongguoPlaybackIdentity, videoID string, now time.Time) string {
	pairs := [][2]string{
		{"ac", "wifi"}, {"channel", "update_64"}, {"aid", "8662"},
		{"app_name", "novelread"}, {"version_code", hongguoVideoTabVersionCode},
		{"version_name", hongguoVideoTabVersionName},
		{"device_platform", "android"}, {"os", "android"}, {"ssmix", "a"},
		{"device_type", "25053RT47C"}, {"device_brand", "Redmi"}, {"language", "zh"},
		{"os_api", "36"}, {"os_version", "16"},
		{"manifest_version_code", hongguoVideoTabVersionCode},
		{"resolution", "1280*2772"}, {"dpi", "520"},
		{"update_version_code", hongguoVideoTabVersionCode},
		{"host_abi", "arm64-v8a"}, {"dragon_device_type", "phone"},
		{"pv_player", hongguoVideoTabVersionCode},
		{"compliance_status", "0"}, {"need_personal_recommend", "1"},
		{"player_so_load", "1"}, {"is_android_pad_screen", "0"},
		{"device_id", identity.DeviceID}, {"iid", identity.InstallID}, {"cdid", identity.CDID},
		{"video_id", videoID}, {"video_id_type", "0"}, {"device_level", "1"},
		{"need_all_video_definition", "1"}, {"need_mp4_align", "0"}, {"use_os_player", "0"},
		{"video_platform", "1024"}, {"source", "0"}, {"from_video_id", ""},
		{"ts", strconv.FormatInt(now.Unix(), 10)},
		{"_rticket", strconv.FormatInt(now.UnixMilli(), 10)},
	}
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, hongguoPlayerEncode(pair[0])+"="+hongguoPlayerEncode(pair[1]))
	}
	return strings.Join(parts, "&")
}

// hongguoShortplayQuery 构造番茄短剧通道 query（aid 1967 / com.dragon.read）。
func hongguoShortplayQuery(identity hongguoPlaybackIdentity) string {
	pairs := [][2]string{
		{"iid", identity.InstallID}, {"device_id", identity.DeviceID},
		{"aid", "1967"}, {"app_name", "novelapp"},
		{"version_code", "72132"}, {"version_name", "7.2.1.32"},
		{"device_platform", "android"}, {"device_brand", "Xiaomi"},
		{"os_version", "13"}, {"cdid", identity.CDID},
	}
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, hongguoPlayerEncode(pair[0])+"="+hongguoPlayerEncode(pair[1]))
	}
	return strings.Join(parts, "&")
}

// 上游对同一出口的高频匿名取址会静默丢弃；参考实现同样用全局 pacing
// 串行化播放取址，且设备身份可能是一次性的。
var (
	hongguoVideoTabMu       sync.Mutex
	hongguoVideoTabNextTime time.Time
)

func hongguoVideoTabPace(ctx context.Context) error {
	hongguoVideoTabMu.Lock()
	start := time.Now()
	if wait := time.Until(hongguoVideoTabNextTime); wait > 0 {
		start = hongguoVideoTabNextTime
	}
	hongguoVideoTabNextTime = start.Add(time.Second)
	wait := time.Until(start)
	hongguoVideoTabMu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// hongguoModelRouteKind 区分取址通道类型。
type hongguoModelRouteKind int

const (
	// hongguoRouteVideoTab：GET /reading/bookapi/video_tab/…（7.3.9.32 设备档案）。
	hongguoRouteVideoTab hongguoModelRouteKind = iota
	// hongguoRouteMultiPlayer：POST /novel/player/multi_video_model/player/v1（现行 App 路径）。
	hongguoRouteMultiPlayer
	// hongguoRouteMultiLegacy：POST /novel/player/multi_video_model/v1/（旧路径，保留兼容）。
	hongguoRouteMultiLegacy
	// hongguoRouteShortplay：POST /novel/player/video_model/v1/（番茄短剧通道）。
	hongguoRouteShortplay
)

type hongguoModelRoute struct {
	kind hongguoModelRouteKind
	base string
}

// hongguoModelRoutes 是取址候选通道，按顺序尝试：主集群 video_tab -> 多集群
// 现行取址路径 -> 旧路径 -> 番茄短剧通道。上游会按集群或出口概率性静默丢弃，
// 串行轮询多个域名比死盯单一域名更容易成功。
var hongguoModelRoutes = []hongguoModelRoute{
	{kind: hongguoRouteVideoTab, base: hongguoPlayerHostPrimary},
	{kind: hongguoRouteMultiPlayer, base: "https://reading.snssdk.com"},
	{kind: hongguoRouteMultiPlayer, base: "https://api5-normal-sinfonlinea.fqnovel.com"},
	{kind: hongguoRouteMultiPlayer, base: "https://api5-normal-lq.fqnovel.com"},
	{kind: hongguoRouteMultiPlayer, base: "https://is.snssdk.com"},
	{kind: hongguoRouteMultiPlayer, base: "https://api5-normal-hl.fqnovel.com"},
	{kind: hongguoRouteMultiLegacy, base: hongguoPlayerHostPrimary},
	{kind: hongguoRouteShortplay, base: "https://api3-normal-sinfonlinea.fqnovel.com"},
}

// getHongguoVideoTabModel 用全新注册的设备身份轮询全部候选通道。
// 仅「静默丢弃」（HTTP 200 + 0 字节）才继续换下一个通道；其余错误直接返回，
// 交给外层兼容回退，避免在上游明确拒绝时反复重试。两轮身份都失败才放弃。
func (downloader *Downloader) getHongguoVideoTabModel(ctx context.Context, videoID string) (map[string]any, error) {
	var lastErr error
	for round := 0; round < 2; round++ {
		if err := hongguoVideoTabPace(ctx); err != nil {
			return nil, err
		}
		identity, err := downloader.registerHongguoPlaybackDevice(ctx)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		for _, route := range hongguoModelRoutes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			result, err := downloader.hongguoModelRouteOnce(ctx, identity, videoID, route)
			if err == nil {
				return result, nil
			}
			lastErr = err
			if !errors.Is(err, errHongguoPlayerEmpty) {
				return nil, err
			}
		}
	}
	return nil, lastErr
}

// hongguoModelRouteOnce 按通道类型发一次取址请求。
func (downloader *Downloader) hongguoModelRouteOnce(ctx context.Context, identity hongguoPlaybackIdentity,
	videoID string, route hongguoModelRoute) (map[string]any, error) {
	switch route.kind {
	case hongguoRouteVideoTab:
		return downloader.hongguoVideoTabGet(ctx, identity, videoID, route.base)
	case hongguoRouteShortplay:
		return downloader.hongguoShortplayModel(ctx, identity, videoID, route.base)
	default:
		return downloader.hongguoMultiModelPost(ctx, identity, videoID, route.base,
			route.kind == hongguoRouteMultiLegacy)
	}
}

// hongguoVideoTabGet 用 7.3.9.32 设备档案发 GET /reading/bookapi/video_tab/…
func (downloader *Downloader) hongguoVideoTabGet(ctx context.Context, identity hongguoPlaybackIdentity,
	videoID, base string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(base, "/")+hongguoVideoTabPath, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", hongguoVideoTabUserAgent)
	request.Header.Set("Accept", "application/json; charset=utf-8,application/x-protobuf")
	request.Header.Set("sdk-version", "2")
	request.Header.Set("x-xs-from-web", "0")
	request.Header.Set("x-ss-dp", "8662")
	request.Header.Set("x-tt-dt", "")
	request.Header.Set("x-tt-store-region", "cn")
	request.Header.Set("x-tt-store-region-src", "did")
	request.Header.Set("passport-sdk-version", "50561")
	request.Header.Set("x-tt-request-tag", "t=0;n=0")

	response, err := downloader.doPreparedCatalogRequest(request, 10*time.Second, func(prepared *http.Request) {
		now := time.Now()
		prepared.URL.RawQuery = hongguoVideoTabQuery(identity, videoID, now)
		// GET 无请求体：551 签名的 stub 按空 body 计算。
		signHongguoPlayerRequest(prepared, nil, now)
	})
	if err != nil {
		return nil, err
	}
	return hongguoVideoTabResponse(request, response)
}

// hongguoMultiModelPost 用 7.4.3.32 档案发 POST 取址请求，请求体 gzip。
// legacy 为 true 时走旧的 /v1/ 路径且不带 dr_scene，其余走现行 /player/v1。
func (downloader *Downloader) hongguoMultiModelPost(ctx context.Context, identity hongguoPlaybackIdentity,
	videoID, base string, legacy bool) (map[string]any, error) {
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
	if !legacy {
		// 现行路径要求标明取址场景为播放器，否则上游按空响应丢弃。
		payload["dr_scene"] = "player"
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(encoded); err != nil {
		writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	body := compressed.Bytes()

	path := hongguoModelPlayerPath
	if legacy {
		path = hongguoModelPath
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", hongguoPlayerUserAgent)
	request.Header.Set("Accept", "application/json; charset=utf-8,application/x-protobuf")
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("sdk-version", "2")
	request.Header.Set("passport-sdk-version", "5051452")
	request.Header.Set("x-xs-from-web", "0")
	request.Header.Set("x-ss-dp", "8662")
	request.Header.Set("x-tt-dt", "")
	request.Header.Set("x-tt-store-region", "cn")
	request.Header.Set("x-tt-store-region-src", "did")
	if legacy {
		request.Header.Set("Connection", "close")
	} else {
		request.Header.Set("x-tt-request-tag", "t=0;n=0")
	}

	response, err := downloader.doPreparedCatalogRequest(request, 10*time.Second, func(prepared *http.Request) {
		now := time.Now()
		prepared.URL.RawQuery = hongguoPlayerQuery(identity.InstallID, identity.DeviceID, identity.CDID, nil, now)
		// POST 的 stub 按 gzip 后的字节计算，与服务端解压前一致。
		signHongguoPlayerRequest(prepared, body, now)
	})
	if err != nil {
		return nil, err
	}
	return hongguoVideoTabResponse(request, response)
}

// hongguoShortplayModel 走番茄短剧通道（com.dragon.read / aid 1967），
// 部分集在上游会作为独立出口返回片源，作为红果取址的最后一条 App 通道。
func (downloader *Downloader) hongguoShortplayModel(ctx context.Context, identity hongguoPlaybackIdentity,
	videoID, base string) (map[string]any, error) {
	payload, err := json.Marshal(map[string]any{
		"video_id":     videoID,
		"content_type": 1004,
		"biz_param": map[string]any{
			"detail_page_version":       0,
			"device_level":              3,
			"disable_digg_stat":         false,
			"disable_video_relate_book": false,
			"from_video_id":             "",
			"need_all_video_definition": true,
			"need_mp4_align":            false,
			"source":                    4,
			"use_os_player":             false,
			"use_server_dns":            false,
			"video_platform":            3,
		},
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+hongguoShortplayPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", hongguoShortplayUserAgent)
	request.Header.Set("Accept", "application/json; charset=utf-8,application/x-protobuf")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("sdk-version", "2")

	response, err := downloader.doPreparedCatalogRequest(request, 10*time.Second, func(prepared *http.Request) {
		now := time.Now()
		prepared.URL.RawQuery = hongguoShortplayQuery(identity)
		signHongguoPlayerRequest(prepared, payload, now)
	})
	if err != nil {
		return nil, err
	}
	result, err := hongguoVideoTabResponse(request, response)
	if err != nil {
		return nil, err
	}
	// 与 multi_video_model 的 data[vid] 结构对齐，后续解析无需区分通道。
	data := nestedMap(result, "data")
	if len(data) == 0 {
		return nil, errors.New("红果短剧通道未返回有效数据")
	}
	if entry := nestedMap(data, videoID); len(entry) == 0 {
		if model := nestedMap(data, "video_model"); len(model) > 0 {
			data[videoID] = map[string]any{"video_model": model}
		}
	}
	return result, nil
}

// hongguoVideoTabResponse 读取响应并把 data.video_model_data 规范化进 data，
// 使后续解析与旧 POST 链路完全同构。
func hongguoVideoTabResponse(request *http.Request, response *http.Response) (map[string]any, error) {
	content, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, &httpStatusError{host: strings.ToLower(request.URL.Hostname()),
			status: response.StatusCode, reason: "红果 App 接口暂不可用"}
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
	data := nestedMap(result, "data")
	if len(data) == 0 {
		data = map[string]any{}
		result["data"] = data
	}
	if entries := nestedMap(data, "video_model_data"); len(entries) > 0 {
		for key, value := range entries {
			data[key] = value
		}
	}
	return result, nil
}
