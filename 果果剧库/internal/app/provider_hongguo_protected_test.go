package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 来自线上 play-info 响应的真实夹具：视频/音频共用同一 player_access_key，
// 派生后应得到 AES-128 密钥 7e13d0dc5023b62eed4371e750b09301（ffmpeg 实测可解密）。
const (
	hongguoProtectedAccessKeyFixture = "oOZ0qW60I61v4HH5a+Uh+zi0dvlq5XX8OdwkxG3cTcZb3UGurg=="
	hongguoProtectedKeyFixture       = "7e13d0dc5023b62eed4371e750b09301"
)

func TestHongguoContentKeyWebVariant(t *testing.T) {
	key, err := hongguoContentKey(hongguoProtectedAccessKeyFixture)
	if err != nil {
		t.Fatalf("web variant derivation failed: %v", err)
	}
	if got := hex.EncodeToString(key); got != hongguoProtectedKeyFixture {
		t.Fatalf("derived key = %s, want %s", got, hongguoProtectedKeyFixture)
	}
	// 减法变体（App 端 tag "11"）无法解析该密钥，确认夹具确实走加法分支。
	raw := mustDecodeFixture(t, hongguoProtectedAccessKeyFixture)
	contentLength := len(raw) - (int(raw[0]^raw[1]^raw[2]) - 48) - 1
	if _, err := deriveHongguoContentKey(raw, contentLength, false); err == nil {
		t.Fatal("subtraction variant should not decode a tag-22 key")
	}
}

func mustDecodeFixture(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := decodeHongguoBase64(value)
	if err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	return raw
}

func hongguoProtectedPlayInfoFixture() map[string]any {
	return map[string]any{
		"vid": "v02ebeg10000db86td27dld3dtuo1rhg", "kid": "6ad06eb4476d5a36f7d80ee30002ebeb",
		"main_url": "https://v26-hgweb.example.test/cfe5a465/video/tos/cn/media-video-avc1/?a=8662",
		"backup_url": "https://v3-hgweb.example.test/df64b46d/video/tos/cn/media-video-avc1/?a=8662",
		"player_access_key": hongguoProtectedAccessKeyFixture,
		"format": "dash", "codec_type": "h264", "definition": "720p",
		"width": "720", "height": "1280", "duration": 147.61,
		"audio": map[string]any{
			"main_url":          "https://v26-hgweb.example.test/9f71495e/video/tos/cn/media-audio-und-mp4a/?a=8662",
			"player_access_key": hongguoProtectedAccessKeyFixture,
			"kid":               "6ad06eb4476d5a36f7d80ee30002ebeb",
		},
	}
}

func TestHongguoProtectedMediaFromResponse(t *testing.T) {
	body, err := json.Marshal(map[string]any{"play_info": hongguoProtectedPlayInfoFixture()})
	if err != nil {
		t.Fatal(err)
	}
	media, err := hongguoProtectedMediaFromResponse(body)
	if err != nil {
		t.Fatalf("parse play-info: %v", err)
	}
	if !strings.Contains(media.URL, "media-video-avc1") {
		t.Errorf("video URL = %q", media.URL)
	}
	if !strings.Contains(media.AudioURL, "media-audio-und-mp4a") {
		t.Errorf("audio URL = %q", media.AudioURL)
	}
	if got := hex.EncodeToString(media.CENCKey); got != hongguoProtectedKeyFixture {
		t.Errorf("CENC key = %s, want %s", got, hongguoProtectedKeyFixture)
	}
	if got := hex.EncodeToString(media.AudioCENCKey); got != hongguoProtectedKeyFixture {
		t.Errorf("audio CENC key = %s, want %s", got, hongguoProtectedKeyFixture)
	}
	if media.Duration != time.Duration(147.61*float64(time.Second)) {
		t.Errorf("duration = %v", media.Duration)
	}
	if media.Quality != 720 || media.Referer != hongguoBaseURL+"/" {
		t.Errorf("quality/referer = %d %q", media.Quality, media.Referer)
	}
	if len(media.Variants) != 1 || !strings.Contains(media.Variants[0].URL, "v3-hgweb") {
		t.Fatalf("backup variant missing: %+v", media.Variants)
	}
	if got := hex.EncodeToString(media.Variants[0].CENCKey); got != hongguoProtectedKeyFixture || media.Variants[0].AudioURL == "" {
		t.Errorf("backup variant lost key or audio track: %+v", media.Variants[0])
	}
}

func TestHongguoProtectedMediaFromResponseLocked(t *testing.T) {
	media, err := hongguoProtectedMediaFromResponse([]byte(`{"error":"protected_playback_unavailable"}`))
	if !errors.Is(err, errHongguoProtectedLocked) {
		t.Fatalf("locked episode error = %v", err)
	}
	if media.URL != "" {
		t.Errorf("locked episode should not return media: %+v", media)
	}
}

func TestHongguoProtectedMediaFromResponseErrors(t *testing.T) {
	cases := map[string]string{
		"invalid json":     `{`,
		"empty play info":  `{"play_info":{}}`,
		"api error":        `{"error":"protected_playback_unavailable2"}`,
		"bad codec":        `{"play_info":{"main_url":"https://x.example.test/v","codec_type":"bytevc2","player_access_key":"` + hongguoProtectedAccessKeyFixture + `","duration":10}}`,
	}
	for name, body := range cases {
		if _, err := hongguoProtectedMediaFromResponse([]byte(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolveHongguoProtectedMediaRequest(t *testing.T) {
	var gotMethod, gotPath, gotContentType, gotReferer, gotOrigin string
	var gotPayload map[string]any
	transport := func(request *http.Request) (*http.Response, error) {
		gotMethod, gotPath = request.Method, request.URL.Path
		gotContentType = request.Header.Get("Content-Type")
		gotReferer = request.Header.Get("Referer")
		gotOrigin = request.Header.Get("Origin")
		if err := json.NewDecoder(request.Body).Decode(&gotPayload); err != nil {
			t.Errorf("payload decode: %v", err)
		}
		body, _ := json.Marshal(map[string]any{"play_info": hongguoProtectedPlayInfoFixture()})
		return rankingHTTPResponse(request, 200, string(body)), nil
	}
	downloader := rankingTestDownloader(t, transport)
	media, err := downloader.resolveHongguoProtectedMedia(context.Background(), "7687485258702261310", "7687486865812753432")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != hongguoProtectedPath {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
	if gotReferer != hongguoBaseURL+"/player/7687485258702261310/7687486865812753432" || gotOrigin != hongguoBaseURL {
		t.Errorf("referer/origin = %q %q", gotReferer, gotOrigin)
	}
	if gotPayload["series_id"] != "7687485258702261310" || gotPayload["item_id"] != "7687486865812753432" || gotPayload["definition"] != "720p" {
		t.Errorf("payload = %+v", gotPayload)
	}
	if hex.EncodeToString(media.CENCKey) != hongguoProtectedKeyFixture || media.AudioURL == "" {
		t.Errorf("media = %+v", media)
	}
}

func TestResolveHongguoProtectedMediaLockedStatus(t *testing.T) {
	var calls int
	transport := func(request *http.Request) (*http.Response, error) {
		calls++
		return rankingHTTPResponse(request, http.StatusForbidden, `{"error":"protected_playback_unavailable"}`), nil
	}
	downloader := rankingTestDownloader(t, transport)
	_, err := downloader.resolveHongguoProtectedMedia(context.Background(), "7687485258702261310", "7687486898897423384")
	if !errors.Is(err, errHongguoProtectedLocked) {
		t.Fatalf("403 + protected_playback_unavailable = %v", err)
	}
	if calls != 1 {
		t.Errorf("locked episode should not be retried, got %d calls", calls)
	}
}

func TestResolveHongguoProtectedMediaRetriesServerErrors(t *testing.T) {
	var calls int
	transport := func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return rankingHTTPResponse(request, http.StatusServiceUnavailable, "<html>busy</html>"), nil
		}
		body, _ := json.Marshal(map[string]any{"play_info": hongguoProtectedPlayInfoFixture()})
		return rankingHTTPResponse(request, 200, string(body)), nil
	}
	downloader := rankingTestDownloader(t, transport)
	downloader.cfg.Retries = 2
	if _, err := downloader.resolveHongguoProtectedMedia(context.Background(), "7687485258702261310", "7687486865812753432"); err != nil {
		t.Fatalf("retry after 5xx failed: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected one retry, got %d calls", calls)
	}
}

func TestPlaybackInputArgsDualTrack(t *testing.T) {
	media := providerMedia{
		URL:          "https://media.example.test/video",
		AudioURL:     "https://media.example.test/audio",
		CENCKey:      []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		AudioCENCKey: []byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
		audioInput:   "http://127.0.0.1:1/nonce/2.m4a",
	}
	args := playbackInputArgs(media, "http://127.0.0.1:1/nonce/1.mp4", 12.5)
	joined := strings.Join(args, " ")
	if strings.Count(joined, "-decryption_key") != 2 {
		t.Errorf("expected per-input decryption keys: %s", joined)
	}
	if strings.Count(joined, "-i ") != 2 {
		t.Errorf("expected two inputs: %s", joined)
	}
	if strings.Count(joined, "-ss 12.500") != 2 {
		t.Errorf("expected seek before each input: %s", joined)
	}
	for _, want := range []string{"-map 0:v:0", "-map 1:a:0?", hex.EncodeToString(media.CENCKey), hex.EncodeToString(media.AudioCENCKey)} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	// 每个输入的解密参数必须紧邻其 -i 之前。
	videoKey := strings.Index(joined, hex.EncodeToString(media.CENCKey))
	videoInput := strings.Index(joined, "-i http://127.0.0.1:1/nonce/1.mp4")
	audioKey := strings.Index(joined, hex.EncodeToString(media.AudioCENCKey))
	audioInput := strings.Index(joined, "-i http://127.0.0.1:1/nonce/2.m4a")
	if !(videoKey < videoInput && videoInput < audioKey && audioKey < audioInput) {
		t.Errorf("input option order wrong: %s", joined)
	}
}

func TestPlaybackInputArgsSingleTrack(t *testing.T) {
	args := playbackInputArgs(providerMedia{URL: "https://media.example.test/video"}, "https://media.example.test/video", 0)
	joined := strings.Join(args, " ")
	if strings.Count(joined, "-i ") != 1 || !strings.Contains(joined, "-map 0:a:0?") {
		t.Errorf("single-track args changed shape: %s", joined)
	}
	if strings.Contains(joined, "-decryption_key") || strings.Contains(joined, "-ss") {
		t.Errorf("unexpected seek/decrypt options: %s", joined)
	}
}

func TestHongguoDramaFromAnyReadsAccessibleEpisodes(t *testing.T) {
	item := map[string]any{"series_id_str": "7687485258702261310", "series_title": "109国道上的烟火", "episode_cnt": "56", "accessible_episode_cnt": "3"}
	drama := hongguoDramaFromAny(item, "真人剧")
	if drama.AccessibleEpisodes != 3 {
		t.Fatalf("accessible episodes = %d, want 3", drama.AccessibleEpisodes)
	}
	if drama.ID != "hongguo:7687485258702261310" || drama.Source != sourceHongguo {
		t.Fatalf("unexpected drama identity: %+v", drama)
	}
	// 字段缺失或非法时应回退为未知（0），以便前端不展示误导性标记。
	for _, bad := range []map[string]any{
		{"series_id_str": "1", "series_title": "x"},
		{"series_id_str": "1", "series_title": "x", "accessible_episode_cnt": "abc"},
		{"series_id_str": "1", "series_title": "x", "accessible_episode_cnt": "-4"},
	} {
		if got := hongguoDramaFromAny(bad, "真人剧").AccessibleEpisodes; got != 0 {
			t.Errorf("invalid accessible value produced %d", got)
		}
	}
}


func TestMergeDramaMetadataKeepsAccessibleEpisodes(t *testing.T) {
	merged := mergeDramaMetadata(Drama{ID: "hongguo:1"}, Drama{AccessibleEpisodes: 3})
	if merged.AccessibleEpisodes != 3 {
		t.Fatalf("accessible episodes not merged: %+v", merged)
	}
	merged = mergeDramaMetadata(Drama{AccessibleEpisodes: 3}, Drama{AccessibleEpisodes: 0})
	if merged.AccessibleEpisodes != 3 {
		t.Fatalf("known boundary lost on merge: %+v", merged)
	}
	merged = mergeDramaMetadata(Drama{AccessibleEpisodes: 2}, Drama{AccessibleEpisodes: 5})
	if merged.AccessibleEpisodes != 5 {
		t.Fatalf("newer boundary should win: %+v", merged)
	}
}
