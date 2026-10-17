package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type mediaByteRange struct {
	start int64
	end   int64
	total int64
}

func parseMediaByteRange(value string) (mediaByteRange, bool) {
	var result mediaByteRange
	unit, value, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return result, false
	}
	span, total, ok := strings.Cut(value, "/")
	if !ok {
		return result, false
	}
	start, end, ok := strings.Cut(span, "-")
	if !ok {
		return result, false
	}
	for index, field := range []string{start, end, total} {
		if field == "" || strings.IndexFunc(field, func(char rune) bool { return char < '0' || char > '9' }) >= 0 {
			return result, false
		}
		number, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return result, false
		}
		switch index {
		case 0:
			result.start = number
		case 1:
			result.end = number
		case 2:
			result.total = number
		}
	}
	return result, result.start <= result.end && result.end < result.total
}

func mediaRequestAttempts(retries int) int {
	return min(3, max(1, retries))
}

// mediaResumeAttempts 返回单集播放允许的续读次数。
//
// 与初始请求的重试次数（mediaRequestAttempts）分开：实测视频 CDN 的单条连接
// 平均只能稳定传输 2~5MB 就被服务端关闭（节点差异大，观测范围 0.2~12.6MB），
// 一集 20MB 左右的剧集通常需要续读 3~5 次才能读完。沿用下载场景的 2 次上限
// 会让大量本可恢复的播放被判死，因此续读单独给一个更宽容的预算。
func mediaResumeAttempts(retries int) int {
	return max(6, mediaRequestAttempts(retries)*2)
}

// mediaResumeStallLimit 限制由「连接静默挂起」触发的续读次数。stall 每次都要等
// 满读超时（30s）才被发现，反复 stall 说明链路本身有问题，继续重试既慢又难奏效，
// 因此单独设一个较小的上限，避免单次播放卡上几分钟。
const mediaResumeStallLimit = 2

// mediaResumeBackoff 返回第 n 次续读前的等待时间。
//
// status == 0 表示传输层失败（连接被对端断开、读超时等），重连成本低，快速重试；
// 带状态码的失败（403 / 429 / 5xx）多半是节点策略，稍作退避换一个节点更可能成功。
func mediaResumeBackoff(attempt, status int) time.Duration {
	if status == 0 {
		return 300 * time.Millisecond
	}
	backoff := time.Duration(attempt) * 600 * time.Millisecond
	if backoff > 1500*time.Millisecond {
		backoff = 1500 * time.Millisecond
	}
	return backoff
}

// retryableMediaStatus 判断续读遇到的状态码是否值得再试一次。
//
// 在原有（408 / 429 / 5xx）基础上放宽到 403 / 404 / 410：视频 CDN 常按节点做
// 防盗链，同一地址落到不同节点时可能对 Range 请求返回 403（实测直连 206 正常、
// 换一个条件即 403），重发一次往往就落到放行节点。
//
// 其余状态一律不再试：200 表示服务端忽略了 Range（重试也不会变成 206），
// 400 / 401 / 416 等属于请求本身有误，重试同样没有意义。
func retryableMediaStatus(status int) bool {
	switch status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone,
		http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

func identityMediaResponse(response *http.Response) bool {
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	return !response.Uncompressed && (encoding == "" || strings.EqualFold(encoding, "identity")) &&
		!strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "multipart/")
}

type resumableMediaReader struct {
	proxy       *hlsProxy
	request     *http.Request
	body        io.ReadCloser
	span        mediaByteRange
	length      int64
	read        int64
	etag        string
	modified    string
	validator   string
	resumable   bool
	attempts    int
	maxAttempts int
	stalls      int
	pending     error
	failure     error
	recovered   bool
}

func (proxy *hlsProxy) mediaReader(request *http.Request, response *http.Response) *resumableMediaReader {
	reader := &resumableMediaReader{
		proxy: proxy, request: request.Clone(request.Context()), body: response.Body,
		length: -1, maxAttempts: mediaResumeAttempts(proxy.retries),
		etag:     strings.TrimSpace(response.Header.Get("ETag")),
		modified: strings.TrimSpace(response.Header.Get("Last-Modified")),
	}
	if response.Request != nil && response.Request.URL != nil {
		reader.request.URL = response.Request.URL
	}
	if response.ContentLength > 0 {
		reader.length = response.ContentLength
	}
	if response.StatusCode == http.StatusOK && reader.length > 0 && response.Header.Get("Content-Range") == "" {
		reader.span = mediaByteRange{end: reader.length - 1, total: reader.length}
		reader.resumable = true
	} else if response.StatusCode == http.StatusPartialContent {
		if span, valid := parseMediaByteRange(response.Header.Get("Content-Range")); valid {
			length := span.end - span.start + 1
			if response.ContentLength < 0 || response.ContentLength == length {
				reader.span, reader.length, reader.resumable = span, length, true
			}
		}
	}
	reader.resumable = reader.resumable && request.Method == http.MethodGet && identityMediaResponse(response)
	if len(reader.etag) >= 2 && strings.HasPrefix(reader.etag, `"`) && strings.HasSuffix(reader.etag, `"`) {
		reader.validator = reader.etag
	} else if _, err := http.ParseTime(reader.modified); err == nil {
		reader.validator = reader.modified
	}
	return reader
}

func (reader *resumableMediaReader) Close() error {
	return reader.body.Close()
}

func (reader *resumableMediaReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for {
		if err := reader.request.Context().Err(); err != nil {
			return 0, err
		}
		if reader.failure != nil {
			return 0, reader.failure
		}
		if reader.length >= 0 && reader.read == reader.length {
			if reader.attempts > 0 && !reader.recovered && reader.proxy.diagnostic != nil {
				reader.recovered = true
				reader.proxy.diagnostic(diagnosticEvent{Level: "info", Event: "media.recovered", Host: reader.request.URL.Hostname(),
					OffsetBytes: reader.span.start + reader.read, TotalBytes: reader.span.total, Attempt: reader.attempts, Message: "媒体续读完成"})
			}
			return 0, io.EOF
		}
		if reader.pending != nil {
			err := reader.pending
			if errors.Is(err, io.EOF) {
				if reader.length < 0 {
					return 0, io.EOF
				}
				err = io.ErrUnexpectedEOF
			}
			if reader.resumable && reader.attempts < reader.maxAttempts {
				if err = reader.resume(); err == nil {
					reader.pending = nil
					continue
				}
			}
			if canceled := reader.request.Context().Err(); canceled != nil {
				return 0, canceled
			}
			reader.failure = fmt.Errorf("媒体资源读取不完整（已读取 %d 字节，续读 %d 次）: %w", reader.read, reader.attempts, err)
			return 0, reader.failure
		}
		if reader.length >= 0 && int64(len(buffer)) > reader.length-reader.read {
			buffer = buffer[:reader.length-reader.read]
		}
		count, err := reader.body.Read(buffer)
		reader.read += int64(count)
		reader.pending = err
		if count > 0 || err == nil {
			return count, nil
		}
	}
}

func (reader *resumableMediaReader) resume() error {
	_ = reader.body.Close()
	reader.body = http.NoBody
	if errors.Is(reader.pending, os.ErrDeadlineExceeded) {
		reader.stalls++
	}
	var lastErr error
	var lastStatus int
	for reader.attempts < reader.maxAttempts && reader.stalls <= mediaResumeStallLimit {
		ctx := reader.request.Context()
		if reader.attempts > 0 {
			timer := time.NewTimer(mediaResumeBackoff(reader.attempts, lastStatus))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		request := reader.request.Clone(ctx)
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", reader.span.start+reader.read, reader.span.end))
		request.Header.Del("If-Range")
		if reader.validator != "" {
			request.Header.Set("If-Range", reader.validator)
		}
		reader.attempts++
		if reader.proxy.diagnostic != nil {
			reader.proxy.diagnostic(diagnosticEvent{Level: "warn", Event: "media.resume", Host: reader.request.URL.Hostname(),
				OffsetBytes: reader.span.start + reader.read, TotalBytes: reader.span.total, Attempt: reader.attempts, Message: "媒体连接中断，正在续读"})
		}
		response, err := reader.proxy.resumeDo(request)
		if err != nil {
			lastErr = err
			continue
		}
		if err = reader.validateContinuation(response); err == nil {
			reader.body = response.Body
			return nil
		}
		_ = response.Body.Close()
		lastErr = err
		lastStatus = response.StatusCode
		if !retryableMediaStatus(response.StatusCode) {
			return err
		}
	}
	return lastErr
}

func (reader *resumableMediaReader) validateContinuation(response *http.Response) error {
	if response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("媒体续读未返回所需字节范围（HTTP %d）", response.StatusCode)
	}
	span, valid := parseMediaByteRange(response.Header.Get("Content-Range"))
	if !valid || span.start != reader.span.start+reader.read || span.end != reader.span.end || span.total != reader.span.total ||
		response.ContentLength >= 0 && response.ContentLength != reader.length-reader.read {
		return errors.New("媒体续读字节范围或长度不一致")
	}
	if !identityMediaResponse(response) || reader.etag != "" && strings.TrimSpace(response.Header.Get("ETag")) != reader.etag ||
		reader.modified != "" && strings.TrimSpace(response.Header.Get("Last-Modified")) != reader.modified {
		return errors.New("媒体续读资源已变化")
	}
	return nil
}
