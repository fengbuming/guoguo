package app

import "strconv"

// 剧库磁盘缓存：内存只保留精简索引条目（dramaEntry），完整 Drama 记录按分片存放磁盘，
// 按需读取并经 LRU 缓存复用。列表/搜索/排序/统计等热路径只访问索引条目，不产生磁盘 IO。

const (
	libraryShardCount = 256
	libraryStoreVersion = 1
)

// dramaEntry 是常驻内存的索引条目。字段名与 Drama 保持一致，便于扫描代码直接复用；
// 仅保留服务端扫描与卡片展示需要的字段，简介、封面别名、分集列表等重字段留在磁盘记录里。
type dramaEntry struct {
	VIP                *bool              `json:"vip,omitempty"`
	ID                 string             `json:"id"`
	Source             string             `json:"source,omitempty"`
	SourceID           string             `json:"sourceId,omitempty"`
	Title              string             `json:"title"`
	Name               string             `json:"name,omitempty"`
	Desc               string             `json:"desc,omitempty"`
	Intro              string             `json:"intro,omitempty"`
	Cover              any                `json:"cover,omitempty"`
	ChannelName        string             `json:"channelName,omitempty"`
	Category           string             `json:"category,omitempty"`
	CategoryName       string             `json:"categoryName,omitempty"`
	TypeName           string             `json:"typeName,omitempty"`
	SortName           string             `json:"sortName,omitempty"`
	Remark             string             `json:"remark,omitempty"`
	Score              string             `json:"score,omitempty"`
	Views              string             `json:"views,omitempty"`
	Heat               string             `json:"heat,omitempty"`
	OnlineDate         string             `json:"onlineDate,omitempty"`
	ReleaseStatus      string             `json:"releaseStatus,omitempty"`
	Tags               []string           `json:"tags,omitempty"`
	EpisodeCount       any                `json:"episodeCount,omitempty"`
	AccessibleEpisodes int                `json:"accessibleEpisodes,omitempty"`
	SortMetadata       *sortMetadataState `json:"sortMetadata,omitempty"`
	// shard 记录完整记录所在分片，仅持久化用，不参与业务比较。
	Shard uint16 `json:"shard"`
}

// newDramaEntry 从完整记录抽出索引条目。调用前应已完成封面归一化（normalizeDramaCover）。
func newDramaEntry(drama Drama) dramaEntry {
	return dramaEntry{
		VIP:                drama.VIP,
		ID:                 drama.ID,
		Source:             drama.Source,
		SourceID:           drama.SourceID,
		Title:              drama.Title,
		Name:               drama.Name,
		Desc:               drama.Desc,
		Intro:              drama.Intro,
		// 封面只保留一份：优先沿用已归一化的 Cover（/api/ui/image 代理地址），
		// 缺失时退回各别名里的原始地址，保证列表/封面代理等热路径无需再读记录文件。
		// 完全没有封面时保持原值（通常为 nil），避免把 nil 变成空字符串。
		Cover:              canonicalDramaCover(drama),
		ChannelName:        drama.ChannelName,
		Category:           drama.Category,
		CategoryName:       drama.CategoryName,
		TypeName:           drama.TypeName,
		SortName:           drama.SortName,
		Remark:             drama.Remark,
		Score:              drama.Score,
		Views:              drama.Views,
		Heat:               drama.Heat,
		OnlineDate:         drama.OnlineDate,
		ReleaseStatus:      drama.ReleaseStatus,
		Tags:               drama.Tags,
		EpisodeCount:       canonicalEpisodeCount(drama),
		AccessibleEpisodes: drama.AccessibleEpisodes,
		SortMetadata:       drama.SortMetadata,
		Shard:              dramaShard(drama.ID),
	}
}

// drama 用磁盘上的完整记录补全索引条目，得到对外使用的 Drama。
// full 为 nil 时仅返回索引条目能覆盖的字段（简介、封面别名等为空）。
func (e *dramaEntry) drama(full *Drama) Drama {
	drama := Drama{
		VIP:                e.VIP,
		ID:                 e.ID,
		Source:             e.Source,
		SourceID:           e.SourceID,
		Title:              e.Title,
		Name:               e.Name,
		Desc:               e.Desc,
		Intro:              e.Intro,
		Cover:              e.Cover,
		ChannelName:        e.ChannelName,
		Category:           e.Category,
		CategoryName:       e.CategoryName,
		TypeName:           e.TypeName,
		SortName:           e.SortName,
		Remark:             e.Remark,
		Score:              e.Score,
		Views:              e.Views,
		Heat:               e.Heat,
		OnlineDate:         e.OnlineDate,
		ReleaseStatus:      e.ReleaseStatus,
		Tags:               e.Tags,
		EpisodeCount:       e.EpisodeCount,
		AccessibleEpisodes: e.AccessibleEpisodes,
		SortMetadata:       e.SortMetadata,
	}
	if full == nil {
		return drama
	}
	// 有完整记录时，封面与集数这类别名字段一律以记录为准，
	// 保证导出、同步包与既有调用方拿到的字段和入库数据完全一致。
	drama.Cover = full.Cover
	drama.EpisodeCount = full.EpisodeCount
	// 磁盘记录补齐索引条目没有的重字段。
	drama.Episodes = full.Episodes
	drama.Duration = full.Duration
	drama.Total = full.Total
	drama.TotalEpisode = full.TotalEpisode
	drama.TotalEpisodeSnake = full.TotalEpisodeSnake
	drama.ChapterCount = full.ChapterCount
	drama.ChapterCountSnake = full.ChapterCountSnake
	drama.EpisodeCountSnake = full.EpisodeCountSnake
	drama.CoverURL = full.CoverURL
	drama.CoverURLSnake = full.CoverURLSnake
	drama.Image = full.Image
	drama.ImageURL = full.ImageURL
	drama.ImageURLSnake = full.ImageURLSnake
	drama.Img = full.Img
	drama.Pic = full.Pic
	drama.Picture = full.Picture
	drama.Poster = full.Poster
	drama.Thumb = full.Thumb
	drama.Thumbnail = full.Thumbnail
	// 记录本身缺封面/集数时退回索引里已算好的值，避免导出丢封面
	if coverString(drama.Cover) == "" && coverString(e.Cover) != "" {
		drama.Cover = e.Cover
	}
	if drama.EpisodeCount == nil {
		drama.EpisodeCount = e.EpisodeCount
	}
	return drama
}

// applyEntry 把索引条目的字段写回完整记录，保证落盘记录与内存一致。
func (e *dramaEntry) applyEntry(drama *Drama) {
	drama.VIP = e.VIP
	drama.ID = e.ID
	drama.Source = e.Source
	drama.SourceID = e.SourceID
	drama.Title = e.Title
	drama.Name = e.Name
	drama.Desc = e.Desc
	drama.Intro = e.Intro
	drama.ChannelName = e.ChannelName
	drama.Category = e.Category
	drama.CategoryName = e.CategoryName
	drama.TypeName = e.TypeName
	drama.SortName = e.SortName
	drama.Remark = e.Remark
	drama.Score = e.Score
	drama.Views = e.Views
	drama.Heat = e.Heat
	drama.OnlineDate = e.OnlineDate
	drama.ReleaseStatus = e.ReleaseStatus
	drama.Tags = e.Tags
	drama.AccessibleEpisodes = e.AccessibleEpisodes
	drama.SortMetadata = e.SortMetadata
}

// entryProvider 返回条目所属站源，语义与 dramaProvider(Drama) 一致。
func entryProvider(entry dramaEntry) string {
	return dramaProvider(Drama{ID: entry.ID, Source: entry.Source})
}

// entryDisplayTitle 返回展示标题，语义与 Drama.DisplayTitle 一致。
func entryDisplayTitle(entry dramaEntry) string {
	if entry.Title != "" {
		return entry.Title
	}
	if entry.Name != "" {
		return entry.Name
	}
	return "短剧"
}

func dramaShard(id string) uint16 {
	if id == "" {
		return 0
	}
	var hash uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		hash ^= uint32(id[i])
		hash *= 16777619
	}
	return uint16(hash % libraryShardCount)
}

func canonicalDramaCover(drama Drama) any {
	if coverString(drama.Cover) != "" {
		return drama.Cover
	}
	if best := bestDramaCover(drama); best != "" {
		return best
	}
	return drama.Cover
}

func coverString(value any) string {
	switch text := value.(type) {
	case string:
		return text
	case map[string]any:
		for _, key := range []string{"url", "src", "path", "cover", "coverUrl", "cover_url"} {
			if inner := coverString(text[key]); inner != "" {
				return inner
			}
		}
	}
	return ""
}

// canonicalEpisodeCount 归一化为单一集数字段，避免多个别名各占一份内存。
func canonicalEpisodeCount(drama Drama) any {
	for _, value := range []any{drama.TotalEpisode, drama.TotalEpisodeSnake, drama.ChapterCount, drama.ChapterCountSnake, drama.EpisodeCount, drama.EpisodeCountSnake, drama.Total} {
		if text := firstNonEmptyString(value); text != "" {
			return text
		}
	}
	return nil
}

func firstNonEmptyString(value any) string {
	switch text := value.(type) {
	case string:
		return text
	case float64:
		if text == float64(int64(text)) {
			return strconv.FormatInt(int64(text), 10)
		}
		return strconv.FormatFloat(text, 'f', -1, 64)
	case int:
		return strconv.Itoa(text)
	}
	return ""
}
