package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type safeDNSDialer struct {
	*dnsResolver
	dialer net.Dialer
}

func newSafeDNSDialer(transport *http.Transport) *safeDNSDialer {
	return &safeDNSDialer{
		dnsResolver: newDNSResolver(transport),
		dialer:      net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
	}
}

// connReadStallTimeout 是单条连接「完全读不到任何数据」的容忍上限。
//
// 背景：手机端局域网播放时，CDN 连接偶发静默挂起（TCP 半开：对端不再发数据，
// 也不回 RST/FIN），body.Read 永久阻塞，既不 EOF 也不报错，媒体续读机制无从触发，
// 表现为「看几集就卡住当前集、无任何提示」。给每次 Read 设 deadline 后，stall
// 超过该阈值即返回 i/o timeout，从而落入既有续读逻辑重新拉流。
//
// 注意这是「空闲」超时而非整体超时：只要连接持续有数据到达（哪怕很慢），每次
// Read 前都会重新顺延 deadline，不会误伤慢速下载或长视频播放。
const connReadStallTimeout = 30 * time.Second

type stallGuardConn struct {
	net.Conn
	timeout time.Duration
}

func (conn *stallGuardConn) Read(buffer []byte) (int, error) {
	if conn.timeout > 0 {
		_ = conn.Conn.SetReadDeadline(time.Now().Add(conn.timeout))
	}
	return conn.Conn.Read(buffer)
}

func wrapStallGuard(connection net.Conn) net.Conn {
	if connection == nil {
		return nil
	}
	return &stallGuardConn{Conn: connection, timeout: connReadStallTimeout}
}

func (resolver *safeDNSDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	connection, err := resolver.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return wrapStallGuard(connection), nil
}

func protectedCDNHost(host string) bool {
	host = strings.ToLower(host)
	return isHuangguoImageCDNHost(host) || strings.HasSuffix(host, ".lkkwip.cn")
}

func (resolver *safeDNSDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !protectedCDNHost(host) {
		return resolver.dial(ctx, network, address)
	}
	addresses, err := resolver.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%s 安全 DNS 解析失败，可在网络设置中启用可用代理: %w", host, err)
	}
	var lastErr error
	for _, resolved := range addresses {
		connection, err := resolver.dial(ctx, network, net.JoinHostPort(resolved, port))
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%s 的 CDN 无法直连，请检查代理配置: %w", host, lastErr)
}
