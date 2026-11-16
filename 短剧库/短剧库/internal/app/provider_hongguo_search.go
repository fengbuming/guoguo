package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	hongguoSearchTimeout      = 40 * time.Second
	hongguoSearchPageTimeout  = 12 * time.Second
	hongguoSearchPendingLimit = 32
	hongguoSearchNameCount    = 50
)

type hongguoSearchEntry struct {
	Dramas    []Drama
	Total     int
	Limited   bool
	Warning   string
	ExpiresAt time.Time
}

type hongguoSearchCall struct {
	done     chan struct{}
	changed  chan struct{}
	entry    hongguoSearchEntry
	snapshot hongguoSearchEntry
	revision uint64
	err      error
}

type hongguoSearchProgressKey struct{}

func reportHongguoSearchProgress(ctx context.Context, entry hongguoSearchEntry) {
	if progress, ok := ctx.Value(hongguoSearchProgressKey{}).(func(hongguoSearchEntry)); ok && len(entry.Dramas) > 0 {
		entry.Total = max(entry.Total, len(entry.Dramas))
		progress(entry)
	}
}

func hongguoSearchKeyword(keyword string) (string, error) {
	keyword = norm.NFKC.String(strings.TrimSpace(keyword))
	if keyword == "" || !utf8.ValidString(keyword) || utf8.RuneCountInString(keyword) > 80 || strings.IndexFunc(keyword, unicode.IsControl) >= 0 {
		return "", errors.New("请输入 1 至 80 个字符的搜索词")
	}
	return keyword, nil
}

func (downloader *Downloader) searchHongguoDramas(ctx context.Context, keyword string) (hongguoSearchEntry, error) {
	return downloader.searchHongguoDramasProgress(ctx, keyword, nil)
}

func (downloader *Downloader) searchHongguoDramasProgress(ctx context.Context, keyword string, progress func(hongguoSearchEntry)) (hongguoSearchEntry, error) {
	keyword, err := hongguoSearchKeyword(keyword)
	if err != nil {
		return hongguoSearchEntry{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, hongguoSearchTimeout)
	defer cancel()
	client := downloader.hongguoClient()
	for {
		if err := ctx.Err(); err != nil {
			return hongguoSearchEntry{}, err
		}
		client.mu.Lock()
		if cached, found := client.searches[keyword]; found && time.Now().Before(cached.ExpiresAt) {
			client.mu.Unlock()
			return cloneHongguoSearchEntry(cached), nil
		}
		if pending := client.searchPending[keyword]; pending != nil {
			client.mu.Unlock()
			entry, err := downloader.waitHongguoSearch(ctx, pending, progress)
			if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() == nil {
				continue
			}
			return entry, err
		}
		if len(client.searchPending) >= hongguoSearchPendingLimit {
			client.mu.Unlock()
			return hongguoSearchEntry{}, errors.New("红果搜索请求较多，请稍后重试")
		}
		pending := &hongguoSearchCall{done: make(chan struct{}), changed: make(chan struct{})}
		client.searchPending[keyword] = pending
		client.mu.Unlock()
		fetchCtx := context.WithValue(ctx, hongguoSearchProgressKey{}, func(entry hongguoSearchEntry) {
			client.mu.Lock()
			pending.snapshot = cloneHongguoSearchEntry(entry)
			pending.revision++
			close(pending.changed)
			pending.changed = make(chan struct{})
			client.mu.Unlock()
			if progress != nil {
				progress(cloneHongguoSearchEntry(entry))
			}
		})
		entry, err := downloader.fetchHongguoSearch(fetchCtx, keyword)
		client.mu.Lock()

		if err == nil && entry.Warning == "" {
			if len(client.searches) >= 64 {
				client.searches = map[string]hongguoSearchEntry{}
			}
			entry.ExpiresAt = time.Now().Add(5 * time.Minute)
			client.searches[keyword] = entry
		}
		pending.entry, pending.err = entry, err
		delete(client.searchPending, keyword)
		close(pending.done)
		client.mu.Unlock()
		return cloneHongguoSearchEntry(entry), err
	}
}

func (downloader *Downloader) waitHongguoSearch(ctx context.Context, pending *hongguoSearchCall, progress func(hongguoSearchEntry)) (hongguoSearchEntry, error) {
	client := downloader.hongguoClient()
	var revision uint64
	for {
		client.mu.Lock()
		changed := pending.changed
		var update *hongguoSearchEntry
		if progress != nil && pending.revision > revision {
			entry := cloneHongguoSearchEntry(pending.snapshot)
			update = &entry
			revision = pending.revision
		}
		client.mu.Unlock()
		if update != nil {
			progress(*update)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				client.mu.Lock()
				partial := cloneHongguoSearchEntry(pending.snapshot)
				client.mu.Unlock()
				if len(partial.Dramas) > 0 {
					partial.Limited = true
					partial.Warning = "搜索已到时限，已保留收到的结果，可重试"
					return partial, nil
				}
			}
			return hongguoSearchEntry{}, ctx.Err()
		case <-pending.done:
			return cloneHongguoSearchEntry(pending.entry), pending.err
		case <-changed:
		}
	}
}

func (downloader *Downloader) fetchHongguoSearch(ctx context.Context, keyword string) (hongguoSearchEntry, error) {

	names, namesErr := downloader.fetchHongguoSearchNames(ctx, keyword)
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && len(names) > 0 {
			return hongguoSearchEntry{Dramas: names, Total: len(names), Limited: true, Warning: "搜索已到时限，已保留名称匹配结果，可重试"}, nil
		}
		return hongguoSearchEntry{}, err
	}
	reportHongguoSearchProgress(ctx, hongguoSearchEntry{Dramas: mergeHongguoSearchDramas([]Drama{}, names), Limited: true})
	pageCtx, cancel := context.WithTimeout(ctx, hongguoSearchPageTimeout)
	defer cancel()
	page, pageErr := downloader.fetchHongguoSearchPage(pageCtx, keyword)
	if errors.Is(ctx.Err(), context.Canceled) {
		return hongguoSearchEntry{}, ctx.Err()
	}
	if pageErr != nil && namesErr != nil || len(page.Dramas)+len(names) == 0 && (pageErr != nil || namesErr != nil) {
		return hongguoSearchEntry{}, errors.Join(pageErr, namesErr)
	}
	entry := hongguoSearchEntry{Dramas: make([]Drama, 0, len(page.Dramas)+len(names)), Total: page.Total, Limited: page.Limited}
	for _, batch := range [][]Drama{page.Dramas, names} {
		entry.Dramas = mergeHongguoSearchDramas(entry.Dramas, batch)
	}
	reportHongguoSearchProgress(ctx, entry)
	seasonsLimited := downloader.completeHongguoSearchSeasons(ctx, keyword, &entry)
	if errors.Is(ctx.Err(), context.Canceled) {
		return hongguoSearchEntry{}, ctx.Err()
	}
	seasonsLimited = seasonsLimited || errors.Is(ctx.Err(), context.DeadlineExceeded)
	sortHongguoSearchDramas(entry.Dramas, keyword)
	if pageErr != nil {
		entry.Warning = "红果综合搜索暂不可用，已保留名称匹配结果，可重试"
	} else if namesErr != nil {
		entry.Warning = "红果名称检索暂不可用，结果可能缺少部分剧集，可重试"
	}
	if seasonsLimited {
		if entry.Warning != "" {
			entry.Warning += "；"
		}
		entry.Warning += "部分季数暂未补齐，已保留当前结果，可重试"
	}
	entry.Limited = entry.Limited || pageErr != nil || namesErr != nil || seasonsLimited
	if entry.Total < len(entry.Dramas) {
		entry.Total = len(entry.Dramas)
	}
	return entry, nil
}

func sortHongguoSearchDramas(dramas []Drama, query string) {
	sort.SliceStable(dramas, func(i, j int) bool {
		return hongguoTitleSearchRank(dramas[i].DisplayTitle(), query) < hongguoTitleSearchRank(dramas[j].DisplayTitle(), query)
	})
	type family struct {
		rank int
		base string
		unit string
	}
	type seasonEntry struct {
		drama  Drama
		number int
	}
	positions := map[family][]int{}
	entries := map[family][]seasonEntry{}
	for index, drama := range dramas {
		title := drama.DisplayTitle()
		base, number, unit, _ := hongguoSearchSeason(title)
		if number < 1 {
			continue
		}
		key := family{hongguoTitleSearchRank(title, query), hongguoSearchText(base), unit}
		positions[key] = append(positions[key], index)
		entries[key] = append(entries[key], seasonEntry{drama, number})
	}
	for key, group := range entries {
		sort.SliceStable(group, func(i, j int) bool { return group[i].number < group[j].number })
		for index, entry := range group {
			dramas[positions[key][index]] = entry.drama
		}
	}
}

func mergeHongguoSearchDramas(dramas, batch []Drama) []Drama {
	positions := make(map[string]int, len(dramas)+len(batch))
	for index, drama := range dramas {
		positions[drama.ID] = index
	}
	for _, drama := range batch {
		if drama.ID == "" {
			continue
		}
		if index, found := positions[drama.ID]; found {
			dramas[index] = mergeDramaMetadata(dramas[index], drama)
		} else {
			positions[drama.ID] = len(dramas)
			dramas = append(dramas, drama)
		}
	}
	return dramas
}

func (downloader *Downloader) fetchHongguoSearchNames(ctx context.Context, keyword string) ([]Drama, error) {
	records, err := downloader.fetchHongguoSuggestionRecords(ctx, keyword, hongguoSearchNameCount)
	if err != nil {
		return nil, err
	}
	return hongguoFilterSearchNames(hongguoSearchNameDramas(records), keyword), nil
}

// hongguoFilterSearchNames 过滤联想结果里与关键词毫无关系的条目。
//
// 联想接口按上游自己的模糊度返回词条：实测搜「天机入梦」会夹带「持械入宋」
// 「晴天放手」，搜「战神」会夹带「退役归来，这个保镖不好惹」。判定标准：
// 标题命中完整关键词，或与关键词有至少两个连续字符重合，且重合段里含中日韩
// 字符（这样「42光年」不会因为共用「42」带出「427元扳倒老油条」，而「v4光年」
// 仍能靠「光年」带出《42光年》）。
func hongguoFilterSearchNames(dramas []Drama, keyword string) []Drama {
	if hongguoSearchText(keyword) == "" {
		return dramas
	}
	filtered := make([]Drama, 0, len(dramas))
	for _, drama := range dramas {
		if hongguoSearchNameRelevant(drama.DisplayTitle(), keyword) {
			filtered = append(filtered, drama)
		}
	}
	return filtered
}

func hongguoSearchNameRelevant(title, keyword string) bool {
	if hongguoSearchTitleMatches(title, keyword) {
		return true
	}
	query := []rune(hongguoSearchText(keyword))
	if len(query) == 0 {
		return false
	}
	normalized := []rune(hongguoSearchText(title))
	if len(query) == 1 {
		return strings.ContainsRune(string(normalized), query[0])
	}
	run := hongguoSearchCommonRun(query, normalized)
	if len(run) < 2 {
		return false
	}
	if !hongguoSearchHasCJK(query) {
		// 关键词本身就是字母数字（zzz、v4），重合段无需含汉字。
		return true
	}
	return hongguoSearchHasCJK(run)
}

// hongguoSearchCommonRun 返回两段文本最长的公共连续子串。
func hongguoSearchCommonRun(left, right []rune) []rune {
	best, bestEnd := 0, 0
	prev := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for i := 1; i <= len(left); i++ {
		for j := 1; j <= len(right); j++ {
			if left[i-1] != right[j-1] {
				current[j] = 0
				continue
			}
			current[j] = prev[j-1] + 1
			if current[j] > best {
				best, bestEnd = current[j], i
			}
		}
		prev, current = current, prev
	}
	return left[bestEnd-best : bestEnd]
}

func hongguoSearchHasCJK(text []rune) bool {
	for _, char := range text {
		if unicode.Is(unicode.Han, char) || unicode.Is(unicode.Hiragana, char) ||
			unicode.Is(unicode.Katakana, char) || unicode.Is(unicode.Hangul, char) {
			return true
		}
	}
	return false
}

// hongguoSuggestionKeyword 提取联想记录 keyword 字段的文本（UseNumber 下为
// json.Number，普通解码为 string），大整数编号不允许走 float 精度损失。
func hongguoSuggestionKeyword(record hongguoSuggestionRecord) string {
	switch value := record.Keyword.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

// hongguoSuggestSeriesID：keyword 提取的编号须为 6 位以上纯数字，
// 避免把纯数字搜索词（common_query）误当剧集编号。
func hongguoSuggestSeriesID(id string) bool {
	return len(id) >= 6 && hongguoNumericID.MatchString(id)
}

// hongguoSuggestDramaWordType：联想词条里可作为剧目的类型白名单。
// 实测（2026-11）short_play_name/title/album 的 keyword 均为剧集编号；
// common_query 是检索词（keyword 为哈希或短语），actor_name 的编号属于演员
// 而非剧集，一律不作为搜索结果。
func hongguoSuggestDramaWordType(wordType string) bool {
	switch wordType {
	case "short_play_name", "title", "album":
		return true
	}
	return false
}

// hongguoSearchNameDramas 把联想记录转成搜索结果。
// 2026-11 联想接口变化：部分剧集（如《42光年》）放在 title 词条，video_data
// 为空、剧集编号记在 keyword 字段；以 word_type 白名单 + 能解析出有效剧集
// 编号（video_data.series_id 或 keyword）为准。
func hongguoSearchNameDramas(records []hongguoSuggestionRecord) []Drama {
	dramas := make([]Drama, 0, len(records))
	for _, record := range records {
		if !hongguoSuggestDramaWordType(record.WordType) {
			continue
		}
		seriesID := firstNonEmpty(mapString(record.VideoData, "series_id_str", "series_id"), hongguoSuggestionKeyword(record))
		if !hongguoSuggestSeriesID(seriesID) {
			continue
		}
		drama := hongguoDramaFromAny(map[string]any{"video_data": record.VideoData, "name": record.Name, "keyword": seriesID}, "短剧")
		if drama.ID != "" && drama.DisplayTitle() != drama.SourceID {
			dramas = append(dramas, drama)
		}
	}
	return dramas
}

func cloneHongguoSearchEntry(entry hongguoSearchEntry) hongguoSearchEntry {
	entry.Dramas = append([]Drama{}, entry.Dramas...)
	for index := range entry.Dramas {
		entry.Dramas[index].Tags = append([]string(nil), entry.Dramas[index].Tags...)
	}
	return entry
}

func hongguoSearchText(text string) string {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, norm.NFKC.String(text))
	if normalized == "" {
		return strings.ToLower(strings.TrimSpace(text))
	}
	return normalized
}

func hongguoTitleSearchRank(title, query string) int {
	words := strings.FieldsFunc(norm.NFKC.String(query), func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
	query = hongguoSearchText(query)
	title = hongguoSearchText(title)
	switch {
	case title == query:
		return 0
	case strings.HasPrefix(title, query):
		return 1
	case strings.Contains(title, query):
		return 2
	default:
		matches := 0
		for _, word := range words {
			if strings.Contains(title, hongguoSearchText(word)) {
				matches++
			}
		}
		if len(words) > 0 && matches == len(words) {
			return 3
		}
		return 4
	}
}

// hongguoSearchAPIPages：综合搜索最多拉取的分页数；接口按热度返回模糊召回，
// 精确匹配主要靠联想名称路线，这里只需要恢复热门召回通道。
const hongguoSearchAPIPages = 3

// hongguoSearchTitleMatches 是综合搜索列表的相关性闸门。
//
// 上游在 query 命中不足时不会返回空列表，而是下发「热门兜底流」：
// 实测 query=42光年 / zzzzzz 都返回 95~108 部毫无关系的热剧（山野春潮、归墟…），
// 且 totalCount 也变成兜底流的条数。整包照收会把这些剧混进搜索结果，
// 表现为「搜出来的东西完全不沾边」。判定标准：标题命中完整关键词
// （NFKC 归一、忽略大小写/空格/标点；多词查询按词全命中）。
func hongguoSearchTitleMatches(title, query string) bool {
	if hongguoSearchText(query) == "" {
		return false
	}
	return hongguoTitleSearchRank(title, query) <= 3
}

// fetchHongguoSearchPage 抓红果综合搜索结果。
// 2026-11-13 起搜索页改为客户端渲染（X-Modernjs-Render: client），HTML 不再
// 内嵌 routerData；页面脚本实际调用匿名 JSON 接口 GET /api/search/result，
// 其 searchList/totalCount 与旧 SSR 数据同源，优先走该接口，旧 HTML 解析
// 保留为兜底。
func (downloader *Downloader) fetchHongguoSearchPage(ctx context.Context, keyword string) (hongguoSearchEntry, error) {
	entry, err := downloader.fetchHongguoSearchAPI(ctx, keyword)
	if err == nil {
		return entry, nil
	}
	return downloader.fetchHongguoSearchHTML(ctx, keyword)
}

type hongguoSearchAPIPayload struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	SearchList []any       `json:"searchList"`
	TotalCount json.Number `json:"totalCount"`
	HasMore    bool        `json:"hasMore"`
}

func (downloader *Downloader) fetchHongguoSearchAPI(ctx context.Context, keyword string) (hongguoSearchEntry, error) {
	base := strings.TrimRight(downloader.providerBaseURL(sourceHongguo), "/")
	entry := hongguoSearchEntry{Dramas: make([]Drama, 0, 20)}
	// trustTotal：第一页整页都是真实命中时，上游 totalCount 才可信。
	trustTotal := false
	for page := 1; page <= hongguoSearchAPIPages; page++ {
		if err := ctx.Err(); err != nil {
			return hongguoSearchEntry{}, err
		}
		params := url.Values{"aid": {"8662"}, "query": {keyword}, "page": {strconv.Itoa(page)}}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/search/result?"+params.Encode(), nil)
		if err != nil {
			if page > 1 {
				break
			}
			return hongguoSearchEntry{}, err
		}
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("Accept", "application/json, text/plain, */*")
		request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		request.Header.Set("Referer", hongguoBaseURL+"/")
		response, err := downloader.doCatalogRequestWithTimeout(request, hongguoSearchPageTimeout)
		if err != nil {
			if page > 1 {
				break
			}
			return hongguoSearchEntry{}, err
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
		closeErr := response.Body.Close()
		if readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			if page > 1 {
				break
			}
			return hongguoSearchEntry{}, readErr
		}
		if len(body) > providerMaxBodyBytes {
			if page > 1 {
				break
			}
			return hongguoSearchEntry{}, fmt.Errorf("红果搜索响应超过 %d 字节", providerMaxBodyBytes)
		}
		if response.StatusCode != http.StatusOK || catalogResponseBlockReason(response, body) != "" {
			if page > 1 {
				break
			}
			return hongguoSearchEntry{}, downloader.catalogResponseError(request, response, body)
		}
		payload, err := parseHongguoSearchAPIResponse(body)
		if err != nil {
			if page > 1 {
				break
			}
			return hongguoSearchEntry{}, err
		}
		matched, total := 0, len(payload.SearchList)
		for _, row := range payload.SearchList {
			drama := hongguoDramaFromAny(row, "短剧")
			if drama.ID == "" {
				total--
				continue
			}
			if !hongguoSearchTitleMatches(drama.DisplayTitle(), keyword) {
				// 命中不足时上游会用热门兜底流填满列表，这些条目与关键词无关。
				continue
			}
			matched++
			entry.Dramas = mergeHongguoSearchDramas(entry.Dramas, []Drama{drama})
		}
		if page == 1 {
			if matched == 0 {
				// 第一页全是兜底流：说明该关键词在上游搜索索引里没有命中，
				// 后续页同样只是兜底，继续拉只会引入更多无关剧集。
				entry.Dramas = nil
				break
			}
			// totalCount 只有在整页都是真实命中时才可信；混了兜底流时它等于
			// 兜底流的条数（实测 ~100），拿去当结果总数会误导用户。
			if trustTotal = matched == total; trustTotal {
				if count, err := payload.TotalCount.Int64(); err == nil {
					entry.Total = int(count)
				}
			}
		}
		if !payload.HasMore {
			break
		}
	}
	entry.Total = max(entry.Total, len(entry.Dramas))
	entry.Limited = entry.Total > len(entry.Dramas)
	if !trustTotal {
		// 兜底流场景下 totalCount 不可信，直接以实际命中数为准。
		entry.Total = len(entry.Dramas)
		entry.Limited = false
	}
	return entry, nil
}

func parseHongguoSearchAPIResponse(body []byte) (hongguoSearchAPIPayload, error) {
	var payload hongguoSearchAPIPayload
	// 剧集编号为 19 位整数：UseNumber 解码避免 float64 精度损失
	// （7648238378667740184 会被舍入成 7648238378667740160）。
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return hongguoSearchAPIPayload{}, fmt.Errorf("红果搜索未返回有效结果: %w", err)
	}
	if payload.Code != "" {
		return hongguoSearchAPIPayload{}, fmt.Errorf("红果搜索暂不可用: %s", firstNonEmpty(payload.Message, payload.Code))
	}
	if payload.SearchList == nil {
		return hongguoSearchAPIPayload{}, errors.New("红果搜索未返回有效结果")
	}
	return payload, nil
}

// fetchHongguoSearchHTML 为 2026-11 改版前的页面解析路径，现仅作兜底。
func (downloader *Downloader) fetchHongguoSearchHTML(ctx context.Context, keyword string) (hongguoSearchEntry, error) {
	base := strings.TrimRight(downloader.providerBaseURL(sourceHongguo), "/")
	pageURL := base + "/search/" + url.PathEscape(keyword)
	body, err := downloader.fetchProviderText(ctx, pageURL, base+"/")
	if err != nil {
		return hongguoSearchEntry{}, err
	}
	page := routerLoaderMap(parseRouterData(body), "search_(keyword)/page", "search_")
	rows, valid := page["searchList"].([]any)
	if page["isSuccess"] != true || !valid || mapString(page, "query") != keyword {
		return hongguoSearchEntry{}, errors.New("红果搜索未返回有效结果")
	}
	entry := hongguoSearchEntry{Dramas: make([]Drama, 0, len(rows))}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if len(nestedMap(row, "video_data")) == 0 {
			continue
		}
		drama := hongguoDramaFromAny(row, "短剧")
		if drama.ID != "" && !seen[drama.ID] {
			seen[drama.ID] = true
			entry.Dramas = append(entry.Dramas, drama)
		}
	}
	if len(rows) > 0 && len(entry.Dramas) == 0 {
		return hongguoSearchEntry{}, errors.New("红果搜索结果中没有可识别的剧集")
	}
	entry.Total, _ = strconv.Atoi(mapString(page, "totalCount"))

	entry.Limited = entry.Total > len(entry.Dramas)
	if entry.Total < len(entry.Dramas) {
		entry.Total = len(entry.Dramas)
	}
	return entry, nil
}
