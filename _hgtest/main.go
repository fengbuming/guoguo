package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"
)

const videoAPI = "https://api5-normal-sinfonlineb.fqnovel.com/novel/player/multi_video_model/v1/"
const detailAPI = "https://api5-normal-sinfonlineb.fqnovel.com/novel/player/video_detail/v1/"

const videoUA = "com.phoenix.read/71332 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3; Cronet/TTNetVersion:04657795 2026-01-23 QuicVersion:c67e9834 2025-09-08)"

// 旧版签名（video_detail 仍可用）
const legacyUA = "com.phoenix.read/73532 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3; Cronet/TTNetVersion:04657795 2026-01-23 QuicVersion:c67e9834 2025-09-08)"

func localID() string {
	return fmt.Sprintf("%d", 1_000_000_000_000_000+rand.Int63n(1<<48))
}

func md5Upper(b []byte) string {
	sum := md5.Sum(b)
	return strings.ToUpper(fmt.Sprintf("%x", sum[:]))
}

func devicePairs(iid, deviceID, khronos, rticket string) [][2]string {
	return [][2]string{
		{"ac", "wifi"}, {"channel", "update_64"}, {"aid", "8662"},
		{"app_name", "novelread"}, {"version_code", "71332"}, {"version_name", "7.1.3.32"},
		{"device_platform", "android"}, {"os", "android"}, {"ssmix", "a"},
		{"device_type", "25053RT47C"}, {"device_brand", "Redmi"}, {"language", "zh"},
		{"os_api", "36"}, {"os_version", "16"}, {"manifest_version_code", "71332"},
		{"resolution", "1280*2772"}, {"dpi", "520"}, {"update_version_code", "71332"},
		{"host_abi", "arm64-v8a"}, {"dragon_device_type", "phone"}, {"pv_player", "71332"},
		{"compliance_status", "0"}, {"need_personal_recommend", "1"}, {"player_so_load", "1"},
		{"is_android_pad_screen", "0"}, {"iid", iid}, {"device_id", deviceID},
		{"ts", khronos}, {"_rticket", rticket},
	}
}

// legacyRequest: 旧签名，用于 video_detail
func legacyRequest(url string, body []byte) (int, []byte, error) {
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	now := time.Now()
	request.URL.RawQuery = "aid=8662&app_name=novelread&version_code=73532&device_platform=android&device_id=" + localID() + "&_rticket=" + fmt.Sprint(now.UnixMilli())
	request.Header.Set("User-Agent", legacyUA)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-XS-From-Web", "0")
	request.Header.Set("Sdk-Version", "2")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	signLegacy(request, body, now)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	return response.StatusCode, content, nil
}

// appSignedPost: 新版六头签名 POST
func appSignedPost(vid string) (map[string]any, error) {
	payload := map[string]any{
		"biz_param": map[string]any{
			"detail_page_version":      0,
			"device_level":             3,
			"disable_digg_stat":        false,
			"need_all_video_definition": true,
			"need_mp4_align":           false,
			"use_os_player":            false,
			"use_server_dns":           false,
			"video_platform":           1024,
		},
		"mixed_video_id_map": map[string]any{"1004": []string{vid}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	khronos := time.Now().Unix()
	pairs := devicePairs(localID(), localID(), fmt.Sprint(khronos), fmt.Sprint(time.Now().UnixMilli()))
	query := encodeQuery(pairs)
	fullURL := videoAPI + "?" + query

	request, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", videoUA)
	request.Header.Set("Accept", "application/json; charset=utf-8,application/x-protobuf")
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	request.Header.Set("x-xs-from-web", "0")
	request.Header.Set("x-ss-req-ticket", fmt.Sprint(time.Now().UnixMilli()))
	request.Header.Set("x-tt-request-tag", "t=0;n=0")
	request.Header.Set("sdk-version", "2")
	request.Header.Set("passport-sdk-version", "50561")
	request.Header.Set("x-vc-bdturing-sdk-version", "3.7.2.cn")
	request.Header.Set("x-ss-stub", md5Upper(body))
	sign := signHeaders(queryOf(fullURL), body, khronos)
	request.Header.Set("x-gorgon", sign.gorgon)
	request.Header.Set("x-khronos", sign.khronos)
	request.Header.Set("x-ladon", sign.ladon)
	request.Header.Set("x-argus", sign.argus)
	request.Header.Set("x-helios", sign.helios)
	request.Header.Set("x-tt-dt", "")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	fmt.Printf("    [POST] %d len=%d\n", response.StatusCode, len(content))
	if len(content) == 0 {
		return nil, fmt.Errorf("空响应")
	}
	var result map[string]any
	if err := json.Unmarshal(content, &result); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w | head=%s", err, string(content[:min(len(content), 200)]))
	}
	return result, nil
}

func getPlain(url string) (map[string]any, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", videoUA)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("fallback HTTP %d", response.StatusCode)
	}
	var result map[string]any
	if err := json.Unmarshal(content, &result); err != nil {
		return nil, fmt.Errorf("fallback JSON 失败: %w", err)
	}
	return result, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	seriesID := os.Getenv("SERIES_ID")
	if seriesID == "" {
		seriesID = "7687485258702261310"
	}
	detailBody, _ := json.Marshal(map[string]any{"series_id": seriesID})
	status, content, err := legacyRequest(detailAPI, detailBody)
	if err != nil || status != 200 || len(content) == 0 {
		fmt.Printf("video_detail 失败 status=%d err=%v len=%d\n", status, err, len(content))
		return
	}
	var detail map[string]any
	json.Unmarshal(content, &detail)
	data, _ := detail["data"].(map[string]any)
	videoData, _ := data["video_data"].(map[string]any)
	videos, _ := videoData["video_list"].([]any)
	var ids []string
	for _, row := range videos {
		item, _ := row.(map[string]any)
		id := fmt.Sprint(item["vid"])
		if id != "" && id != "<nil>" {
			ids = append(ids, id)
		}
	}
	fmt.Printf("series=%s episodes=%d\n", seriesID, len(ids))
	if len(ids) == 0 {
		return
	}
	picks := []int{0, 4, len(ids) - 1}
	for _, index := range picks {
		if index >= len(ids) {
			continue
		}
		vid := ids[index]
		fmt.Printf("\n=== ep%d vid=%s ===\n", index+1, vid)
		result, err := appSignedPost(vid)
		if err != nil {
			fmt.Printf("    FAIL: %v\n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		code := fmt.Sprint(result["code"])
		dataObj, _ := result["data"].(map[string]any)
		fmt.Printf("    code=%s dataKeys=%d\n", code, len(dataObj))
		entry, _ := dataObj[vid].(map[string]any)
		if entry == nil {
			fmt.Printf("    data 无该 vid，keys=%v\n", keysOf(dataObj))
			continue
		}
		model, _ := entry["video_model"].(map[string]any)
		if model == nil {
			if s, ok := entry["video_model"].(string); ok {
				json.Unmarshal([]byte(s), &model)
			}
		}
		if model == nil {
			fmt.Printf("    无 video_model，entry keys=%v\n", keysOf(entry))
			continue
		}
		fallback := ""
		switch v := model["fallback_api"].(type) {
		case string:
			fallback = v
		case []any:
			if len(v) > 0 {
				fallback, _ = v[0].(string)
			}
		case map[string]any:
			fallback, _ = v["fallback_api"].(string)
		}
		fmt.Printf("    fallback=%s\n", truncate(fallback, 110))
		if fallback == "" {
			fmt.Printf("    model keys=%v\n", keysOf(model))
			continue
		}
		outer, err := getPlain(fallback)
		if err != nil {
			fmt.Printf("    fallback FAIL: %v\n", err)
			continue
		}
		if os.Getenv("DUMP") != "" && index == 0 {
			raw, _ := json.MarshalIndent(outer, "", "  ")
			os.WriteFile("fallback.json", raw, 0644)
			fmt.Println("    (已写出 fallback.json)")
		}
		info, _ := outer["video_info"].(map[string]any)
		infoData, _ := info["data"].(map[string]any)
		list, _ := infoData["video_list"].(map[string]any)
		fmt.Printf("    video_info.data.video_list 条目=%d key_seed=%v\n", len(list), infoData["key_seed"] != nil)
		var seed []byte
		if s, ok := infoData["key_seed"].(string); ok && s != "" {
			seed, _ = base64.StdEncoding.DecodeString(s)
		}
		for _, key := range sortedKeys(list) {
			item, _ := list[key].(map[string]any)
			def := fmt.Sprint(item["definition"])
			codec := fmt.Sprint(item["codec_type"])
			encrypted := fmt.Sprint(item["main_url"])
			url := encrypted
			if seed != nil {
				if plain, derr := decryptSpadeURL(encrypted, seed); derr == nil {
					url = plain
				} else {
					url = "DECRYPT-ERR:" + derr.Error()
				}
			}
			if os.Getenv("FIXTURE") != "" && def == "1080p" {
				fmt.Printf("FIXTURE_SEED=%s\nFIXTURE_ENC=%s\nFIXTURE_DEC=%s\nFIXTURE_SPADE=%s\n",
					infoData["key_seed"], encrypted, url, item["spade_a"])
			}
			fmt.Printf("      %-8s %-6s %-9s %vx%v size=%-9v spade=%d\n      url=%s\n",
				key, def, codec, item["vwidth"], item["vheight"], item["size"], len(fmt.Sprint(item["spade_a"])),
				truncate(url, 150))
		}
		if os.Getenv("VERIFY") == "" {
			continue
		}
		// 端到端验证：选 1080p（或最高清晰度），派生密钥，ffmpeg 解密 3 秒
		var chosen map[string]any
		for _, key := range sortedKeys(list) {
			item, _ := list[key].(map[string]any)
			if chosen == nil || fmt.Sprint(item["definition"]) == "1080p" {
				chosen = item
			}
		}
		if chosen == nil {
			continue
		}
		encrypted := fmt.Sprint(chosen["main_url"])
		url := encrypted
		if seed != nil {
			url, _ = decryptSpadeURL(encrypted, seed)
		}
		spade := fmt.Sprint(chosen["spade_a"])
		fmt.Printf("    [VERIFY] %s spade=%s\n", chosen["definition"], spade)
		for _, variant := range []struct {
			name     string
			addition bool
		}{{"subtraction(app)", false}, {"addition(web)", true}} {
			contentKey, tag, kerr := hongguoContentKeyVariant(spade, variant.addition)
			if kerr != nil {
				fmt.Printf("      %-16s FAILED: %v\n", variant.name, kerr)
				continue
			}
			ok := ffmpegProbe(url, hex.EncodeToString(contentKey))
			fmt.Printf("      %-16s tag=%-4q key=%s ffmpeg=%s\n", variant.name, tag, hex.EncodeToString(contentKey), ok)
		}
		time.Sleep(1 * time.Second)
	}
}
