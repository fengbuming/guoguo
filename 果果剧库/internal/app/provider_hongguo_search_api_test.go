package app

import (
	"context"
	"encoding/json"
	"fmt"
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

// 上游在关键词命中不足时不返回空列表，而是用「热门兜底流」把列表填满
// （实测 query=42光年 / zzzzzz 都返回 95~108 部热剧）。整包照收会让搜索结果里
// 混入大量与关键词毫无关系的剧集，因此必须按标题相关性过滤，且兜底流的
// totalCount 不能当作结果总数。
func TestFetchHongguoSearchAPIDropsHotFeedFallback(t *testing.T) {
	newDownloader := func(handler http.HandlerFunc) *Downloader {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return &Downloader{
			cfg:           Config{HongguoURL: server.URL},
			providerHosts: map[string]string{},
			limiter:       newRequestLimiter(4, 0),
			client:        &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second},
		}
	}

	t.Run("mixed page keeps only real hits", func(t *testing.T) {
		var pages []string
		d := newDownloader(func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			pages = append(pages, page)
			switch page {
			case "1":
				_, _ = w.Write([]byte(`{"searchList":[` +
					`{"video_data":{"series_id":"7640888277423098942","series_title":"42光年"}},` +
					`{"video_data":{"series_id":"7671966197985856536","series_title":"持械入宋：第一季"}},` +
					`{"video_data":{"series_id":"7693071659686693913","series_title":"山野春潮"}}` +
					`],"totalCount":108,"hasMore":true}`))
			default:
				_, _ = w.Write([]byte(`{"searchList":[{"video_data":{"series_id":"7638987534483082265","series_title":"归墟第一季"}}],"totalCount":108,"hasMore":false}`))
			}
		})
		entry, err := d.fetchHongguoSearchAPI(context.Background(), "42光年")
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(entry.Dramas) != 1 || entry.Dramas[0].DisplayTitle() != "42光年" {
			t.Fatalf("兜底流混进了搜索结果：%d 部，首个 %q", len(entry.Dramas), entry.Dramas[0].DisplayTitle())
		}
		if entry.Total != 1 || entry.Limited {
			t.Fatalf("兜底流的 totalCount 被当成结果总数：total=%d limited=%v", entry.Total, entry.Limited)
		}
	})

	t.Run("all fallback stops after first page", func(t *testing.T) {
		requests := 0
		d := newDownloader(func(w http.ResponseWriter, r *http.Request) {
			requests++
			_, _ = w.Write([]byte(`{"searchList":[` +
				`{"video_data":{"series_id":"7671966197985856536","series_title":"持械入宋：第一季"}},` +
				`{"video_data":{"series_id":"7638987534483082265","series_title":"归墟第一季"}}` +
				`],"totalCount":95,"hasMore":true}`))
		})
		entry, err := d.fetchHongguoSearchAPI(context.Background(), "zzzzzz")
		if err != nil {
			t.Fatalf("空命中不应报错：%v", err)
		}
		if len(entry.Dramas) != 0 || entry.Total != 0 || entry.Limited {
			t.Fatalf("空命中仍返回了热剧：%d 部 total=%d limited=%v", len(entry.Dramas), entry.Total, entry.Limited)
		}
		if requests != 1 {
			t.Fatalf("兜底流场景不该继续翻页：%d 次请求", requests)
		}
	})
}

// 联想接口按上游模糊度返回词条：搜「天机入梦」会夹带「持械入宋」「晴天放手」，
// 搜「战神」会夹带「退役归来，这个保镖不好惹」。这些与关键词毫无关系的条目
// 不应进入搜索结果，但要保留命中关键词或共享两个字的联想（v4光年 → 42光年）。
func TestHongguoFilterSearchNamesDropsUnrelated(t *testing.T) {
	titles := []string{"天机入梦", "天机当铺：我替天道收债", "奶包入梦", "持械入宋：第一季", "晴天放手"}
	dramas := make([]Drama, 0, len(titles))
	for index, title := range titles {
		dramas = append(dramas, Drama{Source: sourceHongguo, SourceID: fmt.Sprintf("76408882774230989%02d", index), Title: title})
	}
	filtered := hongguoFilterSearchNames(dramas, "天机入梦")
	if len(filtered) != 3 {
		t.Fatalf("联想过滤结果 = %d，want 3: %v", len(filtered), filtered)
	}
	for _, drama := range filtered {
		if strings.Contains(drama.DisplayTitle(), "持械入宋") || drama.DisplayTitle() == "晴天放手" {
			t.Fatalf("无关联想没有被过滤：%q", drama.DisplayTitle())
		}
	}

	// v4光年 与《42光年》共享「4/光/年」三个字符，属于有价值的联想，必须保留。
	keep := hongguoFilterSearchNames([]Drama{{Source: sourceHongguo, SourceID: "7640888277423098942", Title: "42光年"}}, "v4光年")
	if len(keep) != 1 {
		t.Fatal("别名检索能命中的联想被误删")
	}
	// 单字关键词只看是否命中该字。
	if len(hongguoFilterSearchNames([]Drama{{Source: sourceHongguo, SourceID: "1", Title: "暖冬"}}, "冬")) != 1 {
		t.Fatal("单字关键词的联想被误删")
	}
	// 仅共用编号/年份的巧合不算相关：42光年 不该带出 427元扳倒老油条。
	noise := hongguoFilterSearchNames([]Drama{
		{Source: sourceHongguo, SourceID: "2", Title: "427元扳倒老油条"},
		{Source: sourceHongguo, SourceID: "3", Title: "42岁的她逆风翻盘"},
		{Source: sourceHongguo, SourceID: "4", Title: "42光年的回响"},
	}, "42光年")
	if len(noise) != 1 || noise[0].DisplayTitle() != "42光年的回响" {
		t.Fatalf("纯数字重合没有被过滤：%v", noise)
	}
}
