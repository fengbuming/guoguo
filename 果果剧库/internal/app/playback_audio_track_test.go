package app

import (
	"strings"
	"testing"
)

// 红果受保护源的音轨是独立的 DASH 轨道，必须注册成 ffmpeg 的第二路输入。
// 漏掉时 -map 0:a:0? 匹配不到音轨，封装产物只有视频轨，浏览器表现为
// 「完全没声音」并且原生控件把音量按钮禁用。
func TestAttachPlaybackAudioTrackMapsSecondInput(t *testing.T) {
	proxy := &hlsProxy{assets: map[string]hlsAsset{}, assetIDs: map[string]string{}, base: "/api/ui/playback/media/run/1/"}
	media := providerMedia{URL: "https://example.com/video.mp4", AudioURL: "https://example.com/audio.m4a"}
	attachPlaybackAudioTrack(&media, proxy)
	if media.audioInput == "" {
		t.Fatal("分离音轨没有注册进播放代理")
	}
	joined := strings.Join(playbackInputArgs(media, "/tmp/video.mp4", 0), " ")
	if strings.Count(joined, "-i ") != 2 || !strings.Contains(joined, "-map 0:v:0 -map 1:a:0?") {
		t.Fatalf("封装参数没有把音轨作为第二路输入：%s", joined)
	}
	attachPlaybackAudioTrack(&media, proxy)
	if len(proxy.assets) != 1 {
		t.Fatalf("音轨被重复注册：%d", len(proxy.assets))
	}
}

// 单轨媒体（音视频封装在一起）保持原样，不能去等一个不存在的第二路输入。
func TestAttachPlaybackAudioTrackSkipsMuxedMedia(t *testing.T) {
	proxy := &hlsProxy{assets: map[string]hlsAsset{}, assetIDs: map[string]string{}, base: "/api/ui/playback/media/run/1/"}
	media := providerMedia{URL: "https://example.com/video.mp4"}
	attachPlaybackAudioTrack(&media, proxy)
	if media.audioInput != "" || len(proxy.assets) != 0 {
		t.Fatal("单轨媒体不应该注册第二路输入")
	}
	joined := strings.Join(playbackInputArgs(media, "/tmp/video.mp4", 0), " ")
	if !strings.Contains(joined, "-map 0:v:0 -map 0:a:0?") {
		t.Fatalf("单轨媒体应继续从 0:a:0? 取音轨：%s", joined)
	}
}
