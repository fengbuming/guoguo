package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHongguoSearchNameDramasKeywordFallback(t *testing.T) {
	records := []hongguoSuggestionRecord{
		// 2026-11 联想接口形态：title 词条、video_data 为空、编号记在 keyword。
		{Name: "42光年", WordType: "title", Keyword: json.Number("7640888277423098942")},
		{Name: "nova：新星", WordType: "short_play_name", VideoData: map[string]any{"series_id": "7699513062054710296"}},
		{Name: "熊出没MV", WordType: "album", Keyword: "7693518337841581081"},
		// 无编号或白名单之外的词条应被忽略：common_query 是检索词，actor_name
		// 的编号属于演员而非剧集。
		{Name: "vore", WordType: "common_query", Keyword: "vore"},
		{Name: "叠加攻击榜单", WordType: "common_query", Keyword: json.Number("1000000000000000001")},
		{Name: "演员词条", WordType: "actor_name", Keyword: json.Number("1000000000000000002")},
		{Name: "光年", WordType: "title"},
		// 纯数字短词不足以当作剧集编号。
		{Name: "四十二", WordType: "common_query", Keyword: json.Number("42")},
	}
	dramas := hongguoSearchNameDramas(records)
	if len(dramas) != 3 {
		t.Fatalf("dramas = %d, want 3", len(dramas))
	}
	if dramas[0].SourceID != "7640888277423098942" || dramas[0].DisplayTitle() != "42光年" {
		t.Fatalf("first drama = %s / %q", dramas[0].SourceID, dramas[0].DisplayTitle())
	}
	if dramas[1].SourceID != "7699513062054710296" {
		t.Fatalf("second drama = %s", dramas[1].SourceID)
	}
	if dramas[2].SourceID != "7693518337841581081" {
		t.Fatalf("third drama = %s", dramas[2].SourceID)
	}
}

func TestParseHongguoSearchAPIResponse(t *testing.T) {
	valid := `{"searchList":[{"keyword":"7638987534483082265","name":"归墟第一季","video_data":{"series_id":"7638987534483082265","series_title":"归墟第一季","episode_cnt":12}}],"totalCount":193,"hasMore":true,"pageNum":1}`
	payload, err := parseHongguoSearchAPIResponse([]byte(valid))
	if err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if len(payload.SearchList) != 1 || payload.TotalCount.String() != "193" || !payload.HasMore {
		t.Fatalf("payload = %+v", payload)
	}

	empty := `{"searchList":[],"totalCount":0,"hasMore":false}`
	if payload, err = parseHongguoSearchAPIResponse([]byte(empty)); err != nil || payload.SearchList == nil {
		t.Fatalf("empty list should be a valid no-result response: %v", err)
	}

	cases := []string{
		`{"code":"_LIMIT","message":"too many requests"}`,
		`{"code":"_ERROR"}`,
		`{"data":{}}`,
		`<html>client rendered shell</html>`,
		``,
	}
	for _, body := range cases {
		if _, err := parseHongguoSearchAPIResponse([]byte(body)); err == nil {
			t.Fatalf("expected error for %q", body)
		}
	}
}

func TestFetchHongguoSearchAPIPagination(t *testing.T) {
	pages := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/result" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("query") != "光年" {
			http.Error(w, `{"code":"_BAD"}`, http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"searchList":[{"keyword":"100","name":"光年之一","video_data":{"series_id":"100","series_title":"光年之一"}}],"totalCount":31,"hasMore":true}`))
		case "2":
			_, _ = w.Write([]byte(`{"searchList":[{"keyword":"101","name":"光年之二","video_data":{"series_id":"101","series_title":"光年之二"}}],"totalCount":31,"hasMore":true}`))
		case "3":
			http.Error(w, "upstream hiccup", http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"searchList":[],"totalCount":31,"hasMore":false}`))
		}
		pages[r.URL.Query().Get("page")] = "hit"
	}))
	defer server.Close()

	downloader := &Downloader{
		cfg:           Config{HongguoURL: server.URL},
		providerHosts: map[string]string{},
		limiter:       newRequestLimiter(4, 0),
		client:        &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second},
	}
	entry, err := downloader.fetchHongguoSearchAPI(context.Background(), "光年")
	if err != nil {
		t.Fatalf("search api failed: %v", err)
	}
	if len(entry.Dramas) != 2 || entry.Total != 31 || !entry.Limited {
		t.Fatalf("entry = %d dramas, total %d, limited %v", len(entry.Dramas), entry.Total, entry.Limited)
	}
	if _, ok := pages["3"]; !ok {
		t.Fatal("third page was never requested")
	}

	if _, err := downloader.fetchHongguoSearchAPI(context.Background(), "别的"); err == nil {
		t.Fatal("first page failure must surface an error")
	} else if !strings.Contains(err.Error(), "_BAD") && !strings.Contains(err.Error(), "400") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchHongguoSearchPageFallsBackToHTML(t *testing.T) {
	var apiCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/search/result") {
			apiCalled = true
			_, _ = w.Write([]byte(`{"code":"_UNAVAILABLE","message":"api degraded"}`))
			return
		}
		// 旧版 SSR 页面兜底。
		if r.URL.Path == "/search/光年" {
			_, _ = w.Write([]byte(`<html><script>window._ROUTER_DATA={"loaderData":{"search_layout":null,"search_(keyword)/page":{"isSuccess":true,"query":"光年","searchList":[{"keyword":"200","name":"光年旧版","video_data":{"series_id":"200","series_title":"光年旧版"}}],"totalCount":1}}}</script></html>`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	downloader := &Downloader{
		cfg:           Config{HongguoURL: server.URL},
		providerHosts: map[string]string{},
		limiter:       newRequestLimiter(4, 0),
		client:        &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second},
	}
	entry, err := downloader.fetchHongguoSearchPage(context.Background(), "光年")
	if err != nil {
		t.Fatalf("html fallback failed: %v (apiCalled=%v)", err, apiCalled)
	}
	if !apiCalled {
		t.Fatal("api endpoint was not attempted before fallback")
	}
	if len(entry.Dramas) != 1 || entry.Dramas[0].SourceID != "200" {
		t.Fatalf("entry = %+v", entry.Dramas)
	}
}

func TestHongguoSuggestionKeywordTypes(t *testing.T) {
	cases := []struct {
		keyword any
		want    string
	}{
		{json.Number("7640888277423098942"), "7640888277423098942"},
		{"7640888277423098942", "7640888277423098942"},
		{"  spaced  ", "spaced"},
		{42.5, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		record := hongguoSuggestionRecord{Name: "x", Keyword: tc.keyword}
		if got := hongguoSuggestionKeyword(record); got != tc.want {
			t.Fatalf("keyword %v = %q, want %q", tc.keyword, got, tc.want)
		}
	}
}
