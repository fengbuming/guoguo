package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newHuangguoAIPlaybackTestDownloader(base string) *Downloader {
	return &Downloader{
		cfg:           Config{HuangguoAIURL: base},
		providerHosts: map[string]string{},
		limiter:       newRequestLimiter(4, 0),
		client:        &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second},
	}
}

func huangguoAIPlaybackTestTask(pageURL string, episode int) Task {
	return Task{
		DramaID: "huangguoai:3483",
		Index:   1,
		Chapter: Chapter{
			Source:         sourceHuangguoAI,
			PageURL:        pageURL,
			CurrentEpisode: rawEpisode(episode),
		},
	}
}

func TestHuangguoAIEpisodeNumber(t *testing.T) {
	cases := []struct {
		name     string
		pageURL  string
		episode  int
		fallback int
		want     int
	}{
		{"page URL wins", "https://huangguoai.com/video/3483/ep-6/", 2, 1, 6},
		{"query suffix", "https://huangguoai.com/video/3483/ep-3/?autoplay=1", 2, 1, 3},
		{"chapter episode", "https://huangguoai.com/video/3483/", 5, 1, 5},
		{"rawEpisode clamps zero to one", "https://huangguoai.com/detail/3483/", 0, 4, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chapter := huangguoAIPlaybackTestTask(tc.pageURL, tc.episode).Chapter
			if got := huangguoAIEpisodeNumber(chapter, tc.fallback, tc.pageURL); got != tc.want {
				t.Fatalf("episode = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("missing chapter episode uses fallback", func(t *testing.T) {
		chapter := Chapter{Source: sourceHuangguoAI, PageURL: "https://huangguoai.com/detail/3483/"}
		if got := huangguoAIEpisodeNumber(chapter, 4, chapter.PageURL); got != 4 {
			t.Fatalf("episode = %d, want 4", got)
		}
	})
}

func TestParseHuangguoAIPlayback(t *testing.T) {
	valid := `{"status":1,"msg":"","data":{"id":"3483","ep":6,"manifest_method":"GET","streams":{"main":"https://cdn.example.com/videos/a.m3u8?auth_key=abc","hevc":"","preview":""},"cdn":{"m3u8Enabled":true,"authKey":"x-cdn-auth","authValue":"token","app":"150"}}}`
	media, err := parseHuangguoAIPlayback([]byte(valid), "3483", 6, "https://huangguoai.com")
	if err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if media.URL != "https://cdn.example.com/videos/a.m3u8?auth_key=abc" {
		t.Fatalf("URL = %q", media.URL)
	}
	if media.Referer != "https://huangguoai.com/" {
		t.Fatalf("Referer = %q", media.Referer)
	}

	cases := []struct {
		name   string
		body   string
		status bool
	}{
		{"business status", `{"status":0,"msg":"内容已下线"}`, true},
		{"empty streams", `{"status":1,"data":{"id":"3483","ep":6,"manifest_method":"GET","streams":{"main":""}}}`, false},
		{"unsupported manifest", `{"status":1,"data":{"id":"3483","ep":6,"manifest_method":"POST","streams":{"main":"https://cdn.example.com/a.m3u8"}}}`, false},
		{"mismatched id", `{"status":1,"data":{"id":"9999","ep":6,"manifest_method":"GET","streams":{"main":"https://cdn.example.com/a.m3u8"}}}`, false},
		{"mismatched episode", `{"status":1,"data":{"id":"3483","ep":2,"manifest_method":"GET","streams":{"main":"https://cdn.example.com/a.m3u8"}}}`, false},
		{"malformed json", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseHuangguoAIPlayback([]byte(tc.body), "3483", 6, "https://huangguoai.com")
			if err == nil {
				t.Fatal("expected error")
			}
			if got := errors.Is(err, errHuangguoAIPlaybackStatus); got != tc.status {
				t.Fatalf("business error = %v, want %v (err=%v)", got, tc.status, err)
			}
		})
	}

	// id / ep 允许站点返回数字型字段。
	numeric := `{"status":1,"data":{"id":3483,"ep":6,"manifest_method":"GET","streams":{"main":"https://cdn.example.com/a.m3u8"}}}`
	if _, err := parseHuangguoAIPlayback([]byte(numeric), "3483", 6, "https://huangguoai.com"); err != nil {
		t.Fatalf("numeric id/ep rejected: %v", err)
	}
}

func TestFetchHuangguoAIPlaybackRequestShape(t *testing.T) {
	var gotMethod, gotPath, gotBody, gotContentType, gotReferer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(body)
		gotContentType = r.Header.Get("Content-Type")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":1,"msg":"","data":{"id":"3483","ep":6,"manifest_method":"GET","streams":{"main":"https://cdn.example.com/ep6.m3u8?auth_key=xyz"}}}`)
	}))
	defer server.Close()

	d := newHuangguoAIPlaybackTestDownloader(server.URL)
	task := huangguoAIPlaybackTestTask("https://huangguoai.com/video/3483/ep-6/", 6)
	media, err := d.fetchHuangguoAIPlayback(context.Background(), task, task.Chapter)
	if err != nil {
		t.Fatalf("playback request failed: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/videos/3483/episodes/6/playback" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotBody != "{}" || gotContentType != "application/json" {
		t.Fatalf("body = %q, content-type = %q", gotBody, gotContentType)
	}
	if !strings.HasSuffix(gotReferer, "/video/3483/") {
		t.Fatalf("referer = %q", gotReferer)
	}
	if media.URL != "https://cdn.example.com/ep6.m3u8?auth_key=xyz" {
		t.Fatalf("URL = %q", media.URL)
	}
}

func TestFetchHuangguoAIPlaybackFallsBackToNextHost(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":1,"data":{"id":"3483","ep":2,"manifest_method":"GET","streams":{"main":"https://cdn.example.com/ep2.m3u8"}}}`)
	}))
	defer good.Close()

	d := newHuangguoAIPlaybackTestDownloader(bad.URL)
	d.providerHosts[sourceHuangguoAI] = good.URL
	task := huangguoAIPlaybackTestTask("https://huangguoai.com/video/3483/ep-2/", 2)
	media, err := d.fetchHuangguoAIPlayback(context.Background(), task, task.Chapter)
	if err != nil {
		t.Fatalf("fallback host failed: %v", err)
	}
	if media.URL != "https://cdn.example.com/ep2.m3u8" {
		t.Fatalf("URL = %q", media.URL)
	}
}

func TestHuangguoAIPlaybackJSONScalars(t *testing.T) {
	if got := huangguoAIJSONString(json.RawMessage(`"3483"`)); got != "3483" {
		t.Fatalf("string = %q", got)
	}
	if got := huangguoAIJSONString(json.RawMessage(`3483`)); got != "3483" {
		t.Fatalf("number = %q", got)
	}
	if got := huangguoAIJSONInt(json.RawMessage(`"7"`)); got != 7 {
		t.Fatalf("quoted int = %d", got)
	}
	if got := huangguoAIJSONInt(json.RawMessage(`7`)); got != 7 {
		t.Fatalf("int = %d", got)
	}
}
