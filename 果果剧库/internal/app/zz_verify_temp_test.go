package app

import (
	"context"
	"encoding/hex"
	"testing"
	"time"
)

func TestZZVerifyHongguoModelLivePro(t *testing.T) {
	downloader := NewDownloader(Config{Retries: 2})
	for _, item := range []struct{ name, videoID string }{
		{"ep1", "7687486865812753432"},
		{"ep56_最后一集", "7688183650742979646"},
	} {
		media, err := downloader.resolveHongguoModelMedia(context.Background(), item.videoID)
		if err != nil {
			t.Errorf("%s 失败: %v", item.name, err)
			continue
		}
		t.Logf("%s OK quality=%d key=%s url=%.60s", item.name, media.Quality, hex.EncodeToString(media.CENCKey), media.URL)
		task := Task{DramaID: "hongguo:7687485258702261310", Chapter: Chapter{VideoURL: "hongguo-cenc://" + item.videoID}}
		if full, err := downloader.resolveHongguoMedia(context.Background(), task); err != nil {
			t.Errorf("%s resolveHongguoMedia 失败: %v", item.name, err)
		} else {
			t.Logf("%s resolveHongguoMedia OK quality=%d variants=%d", item.name, full.Quality, len(full.Variants))
		}
		time.Sleep(1200 * time.Millisecond)
	}
}
