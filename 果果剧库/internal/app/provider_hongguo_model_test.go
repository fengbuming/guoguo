package app

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// 线上抓到的真实样本（1080p，App 取址链路）
const fixtureSeedB64 = "/ZESJ+vl1HCeBcoCtV1mrv9h2JPlToIf+jcHqkrYFb0="
const fixtureEncryptedURL = "qAABAPpcYnSKxU/G2hrN57c1DtrrbYJNQSx4HwDudCbCl61dmwFJME6HS3Y5KE0y/s7UdW/mgMzJTcZnyz/Xov5gLs2Oj/t+5Fi0vht4RzAHrdDOg/1pvT0GECcEp/ZI6bjTkjLNjz80GyKfy7MOtl4aZiz/pDCOzc5w3y5g6SdgU/8kh+9rXfU4DWm+sqruteJrLvVDUwdQA+1riVVldFYL9/QOVb90riYVGMRAy0T5Lu1uDc61zJE/8priAcz3strvNnSIa9uAvx0asQZ90+6w4j9KI+JcNQTF97H20cK9nym8V8et4bk0TvbUTnXLi6s2hnXDDG13rWvxJE8/7owP0f5wyP9IsZzHwax6dB6IpJ9Cz23L1cwQ38BFyhbfMdLyQMa7Re7fp8sRXgtklbcHgYcLoL4um8t9015bn923L96L5PwKKVzHj9VxPLG2+NZy5ZP7alNb75yAt1nebqJGfF9/vJ9FdjtuDw3rcKZHrzM0gLJrkZ08E9ccm+Ew0DGnzkmwDaFKFl6b7JjjAiizC8pS+6LIf0BEyvGuPkKRZW5TEr7SR/ck/e0v0QirMPjYdTY3svI+mGlO/hMV1953D2WdMU67exPCMxbu5jl+jr4eUYvplQp1CrWJ/kxZI1A9yBxVx8w02NR5x2A4xj3FrF2caXf/POQOcojFDUElKWvOkhdPuTwVYVdBRWGjxZtDQdF8NEENFsyUtcRvfF6A9LL3Piva2rER0QHuOTUgEIlE4R8YGqjCZ5V83C6uv6xzzoxn0/RHIuQUEc0Xgm1eQIgfLND7c1hdOU2aZ6BhDBdsviUJ2A3R3Xb1P4KwSU1k8UUrUreaFRxb56u+5sqxxRPNgZsLbOKI3CPTWQ2OwD9vStmR9JI7iXChIqzyZrctVy5/mjjH1nVXVlGaEC4dqTtMFKLdW/JWxZzUNanmw36HHK+yVygOG2/zoDifsSBcNA=="

// App 端（tag="11"）spade_a → content key，减法变体
const fixtureSpadeA = "lrwYx1COK8lXsBv3Ub0r82CIHPNQuR/wYb4s9le+BfJPtQeDgw=="
const fixtureContentKey = "7e13d0dc5023b62eed4371e750b09301"

func TestHongguoDecryptSpadeURLUsesRealFixture(t *testing.T) {
	seed, err := base64.StdEncoding.DecodeString(fixtureSeedB64)
	if err != nil {
		t.Fatalf("seed 解码失败: %v", err)
	}
	address, err := hongguoDecryptSpadeURL(fixtureEncryptedURL, seed)
	if err != nil {
		t.Fatalf("解密片源地址失败: %v", err)
	}
	if !strings.HasPrefix(address, "https://") || !strings.Contains(address, "qznovelvod.com") {
		t.Fatalf("解密结果不是预期的 CDN 直链: %.80s", address)
	}
	if strings.Contains(address, "mime_type=video_mp4") == false {
		t.Fatalf("解密结果缺少媒体参数: %.80s", address)
	}
}

func TestHongguoDecryptSpadeURLRejectsMalformedInput(t *testing.T) {
	seed, _ := base64.StdEncoding.DecodeString(fixtureSeedB64)
	cases := map[string]string{
		"非 base64":   "not-base64!!",
		"长度不足":       base64.StdEncoding.EncodeToString([]byte{0xa8, 0x01, 0x00}),
		"魔数不符":       base64.StdEncoding.EncodeToString([]byte{0xa9, 0x00, 0x01, 0x00, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}),
		"缺少完整块":      base64.StdEncoding.EncodeToString([]byte{0xa8, 0x00, 0x01, 0x00, 0x01}),
		"解密结果非 URL": base64.StdEncoding.EncodeToString(append([]byte{0xa8, 0x00, 0x01, 0x00}, make([]byte, 16)...)),
	}
	for name, value := range cases {
		if address, err := hongguoDecryptSpadeURL(value, seed); err == nil {
			t.Errorf("%s 应被拒绝，却返回 %q", name, address)
		}
	}
}

func TestHongguoContentKeyAppSubtractionVariant(t *testing.T) {
	key, err := hongguoContentKey(fixtureSpadeA)
	if err != nil {
		t.Fatalf("派生内容密钥失败: %v", err)
	}
	if hex.EncodeToString(key) != fixtureContentKey {
		t.Fatalf("content key = %s, want %s", hex.EncodeToString(key), fixtureContentKey)
	}
}

func TestSelectHongguoModelMediaPrefersH264OverHigherResolution(t *testing.T) {
	list := map[string]any{
		"video_1": map[string]any{
			"main_url": "https://cdn.example.com/a.mp4", "definition": "720p",
			"codec_type": "h264", "vwidth": 720, "vheight": 1280, "size": 100,
		},
		"video_2": map[string]any{
			"main_url": "https://cdn.example.com/b.mp4", "definition": "1080p",
			"codec_type": "bytevc1", "vwidth": 1080, "vheight": 1920, "size": 200,
		},
	}
	media, err := selectHongguoModelMedia(list, nil)
	if err != nil {
		t.Fatalf("选流失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/a.mp4") {
		t.Fatalf("H.264 应优先于更高清晰度，实际选中 %s", media.URL)
	}
	if media.Quality != 720 {
		t.Fatalf("quality = %d, want 720", media.Quality)
	}
	if len(media.Variants) != 2 {
		t.Fatalf("应保留全部候选供回退，实际 %d", len(media.Variants))
	}
}

func TestSelectHongguoModelMediaPicksHighestResolutionWithinCodec(t *testing.T) {
	list := map[string]any{
		"video_1": map[string]any{"main_url": "https://cdn.example.com/360.mp4", "definition": "360p", "codec_type": "bytevc1", "vwidth": 360, "vheight": 640},
		"video_2": map[string]any{"main_url": "https://cdn.example.com/1080.mp4", "definition": "1080p", "codec_type": "bytevc1", "vwidth": 1080, "vheight": 1920},
		"video_3": map[string]any{"main_url": "https://cdn.example.com/540.mp4", "definition": "540p", "codec_type": "bytevc1", "vwidth": 540, "vheight": 960},
	}
	media, err := selectHongguoModelMedia(list, nil)
	if err != nil {
		t.Fatalf("选流失败: %v", err)
	}
	if !strings.HasSuffix(media.URL, "/1080.mp4") {
		t.Fatalf("同编码下应取最高清晰度，实际 %s", media.URL)
	}
}

func TestSelectHongguoModelMediaRejectsUnsupportedCodecAndBadKey(t *testing.T) {
	unsupported := map[string]any{
		"video_1": map[string]any{"main_url": "https://cdn.example.com/a.mp4", "codec_type": "bytevc2"},
	}
	if _, err := selectHongguoModelMedia(unsupported, nil); err == nil {
		t.Fatal("不支持的编码应被跳过并报错")
	}
	badKey := map[string]any{
		"video_1": map[string]any{"main_url": "https://cdn.example.com/a.mp4", "codec_type": "h264", "spade_a": "!!!invalid!!!"},
	}
	if _, err := selectHongguoModelMedia(badKey, nil); err == nil {
		t.Fatal("无法派生的密钥应被跳过并报错")
	}
}

func TestEncodeHongguoModelQueryUsesPythonStyleEscaping(t *testing.T) {
	encoded := encodeHongguoModelQuery([][2]string{{"resolution", "1280*2772"}, {"ac", "wifi"}})
	if !strings.Contains(encoded, "resolution=1280%2A2772") {
		t.Fatalf("星号必须编码为 %%2A，实际 %s", encoded)
	}
	if !strings.Contains(encoded, "ac=wifi") {
		t.Fatalf("普通参数不应被编码，实际 %s", encoded)
	}
	// 签名用的 query 串必须与服务端收到的一致，波浪号保留
	if got := hongguoPyEncode("a~b-c_d.e"); got != "a~b-c_d.e" {
		t.Fatalf("未保留字符被错误编码: %s", got)
	}
}

func TestHongguoModelFallbackAcceptsAllShapes(t *testing.T) {
	target := "https://vas-lf-x.snssdk.com/video/fplay/1/abc"
	cases := []any{
		target,
		[]any{target},
		map[string]any{"fallback_api": target},
		[]any{map[string]any{"fallback_api": target}},
	}
	for index, value := range cases {
		if got := hongguoModelFallback(value); got != target {
			t.Errorf("形态 %d 解析为 %q, want %q", index, got, target)
		}
	}
	if hongguoModelFallback(nil) != "" || hongguoModelFallback(map[string]any{}) != "" {
		t.Fatal("缺失 fallback 应返回空串")
	}
}

func TestSignHongguoModelProducesAllHeaders(t *testing.T) {
	signature := signHongguoModel("aid=8662&_rticket=1700000000000", []byte(`{"a":1}`), 1700000000)
	if !strings.HasPrefix(signature.gorgon, "8404") {
		t.Fatalf("x-gorgon 前缀错误: %s", signature.gorgon)
	}
	// 2 字节魔数 + 4 字节随机/标志 + 20 字节负载，十六进制共 52 个字符
	raw, err := hex.DecodeString(signature.gorgon)
	if err != nil {
		t.Fatalf("x-gorgon 不是合法十六进制: %v", err)
	}
	if len(raw) != 26 {
		t.Fatalf("x-gorgon 解码后 = %d 字节, want 26", len(raw))
	}
	if signature.khronos != "1700000000" {
		t.Fatalf("x-khronos = %s", signature.khronos)
	}
	ladon, errL := base64.StdEncoding.DecodeString(signature.ladon)
	argus, errA := base64.StdEncoding.DecodeString(signature.argus)
	if errL != nil || errA != nil {
		t.Fatalf("ladon/argus 不是合法 base64: %v %v", errL, errA)
	}
	if len(ladon) != 4 || len(argus) != 4 {
		t.Fatalf("ladon/argus 应为 4 字节，实际 %d/%d", len(ladon), len(argus))
	}
	// 大端与小端互为逆序
	if ladon[0] != argus[3] || ladon[3] != argus[0] {
		t.Fatalf("ladon/argus 字节序不符: %x / %x", ladon, argus)
	}
	if signature.helios == "" {
		t.Fatal("x-helios 为空")
	}
}

func TestHongguoModelQueryPairsCarryRequiredFields(t *testing.T) {
	pairs := hongguoModelQueryPairs("111", "222", "1700000000", "1700000000000")
	values := map[string]string{}
	for _, pair := range pairs {
		values[pair[0]] = pair[1]
	}
	for _, key := range []string{"aid", "app_name", "device_id", "iid", "ts", "_rticket", "host_abi", "pv_player"} {
		if values[key] == "" {
			t.Errorf("缺少必需参数 %s", key)
		}
	}
	if values["aid"] != "8662" || values["app_name"] != "novelread" {
		t.Errorf("基础参数不符: aid=%s app_name=%s", values["aid"], values["app_name"])
	}
}

func TestHongguoModelMediaAddressDecryptsWithSeed(t *testing.T) {
	seed, _ := base64.StdEncoding.DecodeString(fixtureSeedB64)
	row := map[string]any{"main_url": fixtureEncryptedURL}
	address := hongguoModelMediaAddress(row, seed)
	if !strings.Contains(address, "qznovelvod.com") {
		t.Fatalf("带 seed 时应解密出直链，实际 %.60s", address)
	}
	// 无 seed 时密文不可用作直链，应回退为空
	if got := hongguoModelMediaAddress(row, nil); got != "" {
		t.Fatalf("无 seed 时不该返回密文，实际 %.40s", got)
	}
	// backup 字段同样可用
	backup := map[string]any{"backup_url_1": fixtureEncryptedURL}
	if got := hongguoModelMediaAddress(backup, seed); !strings.Contains(got, "qznovelvod.com") {
		t.Fatalf("backup_url_1 未被使用，实际 %.60s", got)
	}
}

func TestHongguoModelRequestSurfacesEmptyBodyAsError(t *testing.T) {
	// 上游签名被拒时返回 200 + 空 body，不能当成成功
	if got := hongguoModelAsObject("not-json"); len(got) != 0 {
		t.Fatalf("非 JSON 字符串应返回空 map，实际 %v", got)
	}
	if got := hongguoModelAsObject(`{"fallback_api":"https://x.example.com/a"}`); got["fallback_api"] == nil {
		t.Fatal("嵌套 JSON 字符串应被解析")
	}
	if status := (&httpStatusError{host: "api5-normal-sinfonlineb.fqnovel.com", status: http.StatusOK, reason: "x"}); status.Error() == "" {
		t.Fatal("httpStatusError 应带可读信息")
	}
}
