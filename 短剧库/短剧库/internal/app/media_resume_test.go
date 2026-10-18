package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mediaProxyFixture(t *testing.T, handler http.HandlerFunc) (*hlsProxy, *http.Client) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	downloader := &Downloader{cfg: Config{Retries: 3}, client: server.Client()}
	proxy, err := downloader.newHLSProxy(context.Background(), providerMedia{URL: server.URL + "/fixture.mp4", Referer: "https://example.invalid/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return proxy, client
}

func TestMediaProxyResumesTruncatedResponse(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64)
	for _, test := range []struct {
		name        string
		rangeHeader string
		start       int
		end         int
	}{
		{name: "whole", start: 0, end: 1023},
		{name: "bounded seek", rangeHeader: "bytes=100-699", start: 100, end: 699},
		{name: "suffix seek", rangeHeader: "bytes=-129", start: 895, end: 1023},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				attempt := calls.Add(1)
				if request.Header.Get("Accept-Encoding") != "identity" || request.Header.Get("Referer") != "https://example.invalid/" {
					t.Error("media request lost source headers")
				}
				start, end, status := test.start, test.end, http.StatusOK
				if test.rangeHeader != "" {
					status = http.StatusPartialContent
				}
				if attempt == 1 {
					if request.Header.Get("Range") != test.rangeHeader {
						t.Error("initial seek range was changed")
					}
				} else {
					start += 13
					status = http.StatusPartialContent
					want := fmt.Sprintf("bytes=%d-%d", start, end)
					if request.Header.Get("Range") != want || request.Header.Get("If-Range") != `"fixture-v1"` {
						t.Errorf("unsafe continuation: Range=%q If-Range=%q, want %s", request.Header.Get("Range"), request.Header.Get("If-Range"), want)
					}
				}
				writer.Header().Set("ETag", `"fixture-v1"`)
				writer.Header().Set("Content-Type", "video/mp4")
				writer.Header().Set("Content-Length", strconv.Itoa(end-start+1))
				if status == http.StatusPartialContent {
					writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
				}
				writer.WriteHeader(status)
				if attempt == 1 {
					_, _ = writer.Write(payload[start : start+13])
				} else {
					_, _ = writer.Write(payload[start : end+1])
				}
			})
			request, _ := http.NewRequest(http.MethodGet, proxy.root, nil)
			request.Header.Set("Range", test.rangeHeader)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(body, payload[test.start:test.end+1]) || calls.Load() != 2 || proxy.Err() != nil {
				t.Fatalf("interrupted media did not resume exactly: bytes=%d calls=%d read=%v proxy=%v", len(body), calls.Load(), err, proxy.Err())
			}
			wantStatus := http.StatusOK
			if test.rangeHeader != "" {
				wantStatus = http.StatusPartialContent
			}
			if response.StatusCode != wantStatus || response.ContentLength != int64(test.end-test.start+1) {
				t.Fatal("continuation changed the local response headers")
			}
		})
	}
}

func TestMediaProxyExhaustsBodyRetries(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 10)
	var calls atomic.Int32
	proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		attempt := int(calls.Add(1))
		start := (attempt - 1) * 13
		writer.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
		writer.Header().Set("ETag", `"fixture-v1"`)
		if attempt > 1 {
			if request.Header.Get("Range") != fmt.Sprintf("bytes=%d-99", start) {
				t.Error("retry did not preserve its cumulative offset")
			}
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-99/100", start))
			writer.WriteHeader(http.StatusPartialContent)
		}
		_, _ = writer.Write(payload[start : start+13])
	})
	response, err := client.Get(proxy.root)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// 传输层失败（连接被对端断开）退避很短但仍有上限：首次 + 6 次续读，
	// 每次续读只推进 13 字节，因此最终只拿到 7*13 字节后必须报失败。
	body, err := io.ReadAll(response.Body)
	if err == nil || calls.Load() != 7 || !bytes.Equal(body, payload[:91]) || proxy.Err() == nil {
		t.Fatalf("exhausted transfer was not bounded or reported: bytes=%d calls=%d read=%v proxy=%v", len(body), calls.Load(), err, proxy.Err())
	}
}

func TestMediaProxyRejectsUnsafeContinuation(t *testing.T) {
	for _, test := range []struct {
		name, contentRange, etag, encoding string
		status, length                     int
	}{
		{name: "range ignored", status: 200, length: 100, etag: `"fixture-v1"`},
		{name: "wrong start", status: 206, length: 90, contentRange: "bytes 9-98/100", etag: `"fixture-v1"`},
		{name: "wrong end", status: 206, length: 89, contentRange: "bytes 10-98/100", etag: `"fixture-v1"`},
		{name: "changed length", status: 206, length: 90, contentRange: "bytes 10-99/101", etag: `"fixture-v1"`},
		{name: "changed object", status: 206, length: 90, contentRange: "bytes 10-99/100", etag: `"fixture-v2"`},
		{name: "missing validator", status: 206, length: 90, contentRange: "bytes 10-99/100"},
		{name: "encoded bytes", status: 206, length: 90, contentRange: "bytes 10-99/100", etag: `"fixture-v1"`, encoding: "gzip"},
		{name: "conflicting length", status: 206, length: 80, contentRange: "bytes 10-99/100", etag: `"fixture-v1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				if calls.Add(1) == 1 {
					writer.Header().Set("Content-Length", "100")
					writer.Header().Set("ETag", `"fixture-v1"`)
					_, _ = io.WriteString(writer, "0123456789")
					return
				}
				writer.Header().Set("Content-Length", strconv.Itoa(test.length))
				writer.Header().Set("Content-Range", test.contentRange)
				writer.Header().Set("ETag", test.etag)
				writer.Header().Set("Content-Encoding", test.encoding)
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, strings.Repeat("x", test.length))
			})
			response, err := client.Get(proxy.root)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err == nil || string(body) != "0123456789" || calls.Load() != 2 || proxy.Err() == nil {
				t.Fatalf("unverified bytes were appended or failure was lost: bytes=%d calls=%d read=%v proxy=%v", len(body), calls.Load(), err, proxy.Err())
			}
		})
	}
}

func TestMediaProxyCanceledRequestDoesNotRecordFailure(t *testing.T) {
	proxy, _ := mediaProxyFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("canceled request reached upstream")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, proxy.root, nil).WithContext(ctx)
	proxy.ServeHTTP(httptest.NewRecorder(), request)
	if proxy.Err() != nil {
		t.Fatal("stopping playback poisoned its media proxy", proxy.Err())
	}
}

func TestMediaProxyCancelsDuringContinuation(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	proxy, _ := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(writer, "0123456789")
			return
		}
		close(started)
		<-request.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, proxy.root, nil).WithContext(ctx))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("continuation never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("continuation blocked playback cancellation")
	}
	if proxy.Err() != nil || calls.Load() != 2 {
		t.Fatal("cancellation was recorded as an upstream failure", proxy.Err())
	}
}

func TestMediaProxyRetriesTemporaryContinuationStatus(t *testing.T) {
	const modified = "Mon, 14 Sep 2026 12:00:00 GMT"
	var calls atomic.Int32
	proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		attempt := calls.Add(1)
		if attempt == 2 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Last-Modified", modified)
		if attempt == 1 {
			writer.Header().Set("Content-Length", "20")
			_, _ = io.WriteString(writer, "0123456789")
			return
		}
		if request.Header.Get("Range") != "bytes=10-19" || request.Header.Get("If-Range") != modified {
			t.Error("date validator or retry offset was lost")
		}
		writer.Header().Set("Content-Length", "10")
		writer.Header().Set("Content-Range", "bytes 10-19/20")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(writer, "abcdefghij")
	})
	response, err := client.Get(proxy.root)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "0123456789abcdefghij" || calls.Load() != 3 || proxy.Err() != nil {
		t.Fatal("temporary continuation failure was latched", err, proxy.Err())
	}
}

func TestMediaProxyHeadAndUnknownLength(t *testing.T) {
	for _, mode := range []string{"head", "chunked", "broken chunked"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if mode == "head" {
					writer.Header().Set("Content-Length", "100")
					return
				}
				writer.WriteHeader(http.StatusOK)
				writer.(http.Flusher).Flush()
				_, _ = io.WriteString(writer, "unknown-length-fixture")
				writer.(http.Flusher).Flush()
				if mode == "broken chunked" {
					connection, _, err := writer.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
				}
			})
			method := http.MethodGet
			if mode == "head" {
				method = http.MethodHead
			}
			request, _ := http.NewRequest(method, proxy.root, nil)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if mode == "head" && len(body) != 0 || mode != "head" && string(body) != "unknown-length-fixture" || calls.Load() != 1 {
				t.Fatal("HEAD or unknown-length response changed")
			}
			if (proxy.Err() != nil) != (mode == "broken chunked") {
				t.Fatal("unverifiable truncation was hidden or a complete body was rejected", proxy.Err())
			}
		})
	}
}

type mediaFailureBody struct {
	data []byte
}

func (body *mediaFailureBody) Read(buffer []byte) (int, error) {
	count := copy(buffer, body.data)
	body.data = body.data[count:]
	return count, io.ErrUnexpectedEOF
}

func (*mediaFailureBody) Close() error { return nil }

type mediaStoppedWriter struct{ header http.Header }

func (writer *mediaStoppedWriter) Header() http.Header { return writer.header }
func (*mediaStoppedWriter) WriteHeader(int)            {}
func (*mediaStoppedWriter) Write([]byte) (int, error)  { return 0, errors.New("consumer stopped") }

func TestMediaProxyConsumerStopDoesNotLatchPendingReadError(t *testing.T) {
	proxy, _ := mediaProxyFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("fixture transport was bypassed")
	})
	var calls int
	proxy.client.Transport = rankingTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {"100"}}, ContentLength: 100,
			Body: &mediaFailureBody{data: []byte("0123456789")}, Request: request}, nil
	})
	proxy.ServeHTTP(&mediaStoppedWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodGet, proxy.root, nil))
	if calls != 1 || proxy.Err() != nil {
		t.Fatal("unused media bytes triggered a retry or false failure", proxy.Err())
	}
}

func TestMediaReaderAcceptsExactlyCompletedBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/media", nil)
	proxy := &hlsProxy{retries: 3}
	reader := proxy.mediaReader(request, &http.Response{StatusCode: 200, ContentLength: 10, Header: make(http.Header), Body: &mediaFailureBody{data: []byte("0123456789")}})
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "0123456789" || reader.attempts != 0 {
		t.Fatal("completed response was retried", err)
	}
}

// 403 多为 CDN 按节点做的防盗链拒绝：同一个地址落到放行节点就能恢复，
// 因此续读遇到 403 应当再试，而不是立刻判死。
func TestMediaProxyRetriesForbiddenContinuation(t *testing.T) {
	var calls atomic.Int32
	proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		switch calls.Add(1) {
		case 1: // 首次：声明 100 字节但只给 10 字节，制造中断
			writer.Header().Set("Content-Length", "100")
			writer.Header().Set("ETag", `"fixture-v1"`)
			_, _ = io.WriteString(writer, "0123456789")
		case 2: // 第一次续读：模拟防盗链节点返回 403
			writer.WriteHeader(http.StatusForbidden)
		default: // 第二次续读：落到放行节点
			writer.Header().Set("Content-Length", "90")
			writer.Header().Set("Content-Range", "bytes 10-99/100")
			writer.Header().Set("ETag", `"fixture-v1"`)
			writer.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(writer, strings.Repeat("x", 90))
		}
	})
	response, err := client.Get(proxy.root)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("403 后应重试并续读成功: %v", err)
	}
	if len(body) != 100 || !strings.HasPrefix(string(body), "0123456789") {
		t.Fatalf("续读结果不完整: len=%d", len(body))
	}
	if calls.Load() != 3 {
		t.Fatalf("续读次数 = %d, want 3（首次 + 403 + 成功）", calls.Load())
	}
}

// 一直被拒时仍要给出失败原因，且重试次数有上限，不能无限打上游。
func TestMediaProxyGivesUpAfterRepeatedForbiddenContinuation(t *testing.T) {
	var calls atomic.Int32
	proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Content-Length", "100")
			writer.Header().Set("ETag", `"fixture-v1"`)
			_, _ = io.WriteString(writer, "0123456789")
			return
		}
		writer.WriteHeader(http.StatusForbidden)
	})
	response, err := client.Get(proxy.root)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("持续 403 时应返回失败")
	}
	if proxy.Err() == nil {
		t.Fatal("持续 403 时应记录失败原因")
	}
	// Retries=3 → 续读预算 6 次：首次 + 6 次续读，之后必须放弃并给出原因
	if calls.Load() != 7 {
		t.Fatalf("请求次数 = %d, want 7", calls.Load())
	}
}

// 续读预算与初始请求重试解耦：一集 20MB 常需续读 3~5 次，2 次预算远远不够。
func TestMediaResumeBudgetAndBackoff(t *testing.T) {
	if got := mediaResumeAttempts(3); got != 6 {
		t.Fatalf("mediaResumeAttempts(3) = %d, want 6", got)
	}
	if got := mediaResumeAttempts(1); got < 6 {
		t.Fatalf("mediaResumeAttempts(1) = %d, want >= 6", got)
	}
	// 传输层失败（连接被断）重连成本低，固定短退避
	if got := mediaResumeBackoff(4, 0); got != 300*time.Millisecond {
		t.Fatalf("transport backoff = %v, want 300ms", got)
	}
	// 服务端拒绝则递增退避并封顶，避免每次都撞同一个节点
	if got := mediaResumeBackoff(1, http.StatusForbidden); got != 600*time.Millisecond {
		t.Fatalf("status backoff(1) = %v, want 600ms", got)
	}
	if got := mediaResumeBackoff(9, http.StatusServiceUnavailable); got != 1500*time.Millisecond {
		t.Fatalf("status backoff(9) = %v, want 1500ms", got)
	}
}

// 续读必须走不复用连接的专用客户端：复用到服务端已关闭的陈旧连接会以
// 「刚续读几十 KB 就 EOF」的形式白白吃掉一次续读预算。
func TestMediaProxyUsesResumeClientForContinuation(t *testing.T) {
	var resumeCalls atomic.Int32
	proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "100")
		writer.Header().Set("ETag", `"fixture-v1"`)
		_, _ = io.WriteString(writer, "0123456789")
	})
	proxy.resumeClient = &http.Client{Transport: rankingTransport(func(request *http.Request) (*http.Response, error) {
		resumeCalls.Add(1)
		return nil, errors.New("续读走专用客户端")
	})}
	response, err := client.Get(proxy.root)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("续读全部失败时应返回错误")
	}
	if resumeCalls.Load() != 6 {
		t.Fatalf("续读请求次数 = %d, want 6（走专用客户端且用满预算）", resumeCalls.Load())
	}
}

func TestRetryableMediaStatusCoversTransientRejections(t *testing.T) {
	retry := []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone,
		http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable}
	for _, status := range retry {
		if !retryableMediaStatus(status) {
			t.Errorf("HTTP %d 应重试", status)
		}
	}
	// 服务端忽略 Range（200）或请求本身有误时，重试没有意义
	skip := []int{http.StatusOK, http.StatusPartialContent, http.StatusBadRequest,
		http.StatusUnauthorized, http.StatusRequestedRangeNotSatisfiable}
	for _, status := range skip {
		if retryableMediaStatus(status) {
			t.Errorf("HTTP %d 不该重试", status)
		}
	}
}

// 当前 CDN 节点持续异常时，续读应换到备用节点而不是死磕；
// 换节点时校验器已重置，因此备用节点即使 ETag 不同也能续读成功。
func TestMediaProxyFailsOverToAlternateHost(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 10)
	var primaryCalls, backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		backupCalls.Add(1)
		writer.Header().Set("Content-Range", "bytes 10-99/100")
		writer.Header().Set("ETag", `"alt-v2"`)
		writer.Header().Set("Content-Length", "90")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(payload[10:])
	}))
	t.Cleanup(backup.Close)
	proxy, client := mediaProxyFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		primaryCalls.Add(1)
		if request.Header.Get("Range") == "" {
			writer.Header().Set("Content-Length", "100")
			writer.Header().Set("ETag", `"primary-v1"`)
			_, _ = writer.Write(payload[:10])
			return
		}
		writer.WriteHeader(http.StatusForbidden)
	})
	for key, asset := range proxy.assets {
		asset.fallbacks = []hlsAsset{{remote: backup.URL + "/fixture.mp4"}}
		proxy.assets[key] = asset
	}
	response, err := client.Get(proxy.root)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("换节点后应续读成功: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("续读内容不完整: %d/%d 字节", len(body), len(payload))
	}
	if backupCalls.Load() != 1 {
		t.Fatalf("备用节点应只被请求一次，实际 %d", backupCalls.Load())
	}
	if primaryCalls.Load() > 3 {
		t.Fatalf("主节点最多 1 次初始 + %d 次续读，实际 %d", mediaResumeAttemptsPerHost, primaryCalls.Load())
	}
}
