package app

import (
	"context"
	"net"
	"time"
)

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

// newStallGuardDialContext 返回包过 stall guard 的 DialContext，装在主 transport 上，
// 使播放/下载等长连接在静默挂起时能以 i/o timeout 失败并触发续读。
func newStallGuardDialContext() func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return wrapStallGuard(connection), nil
	}
}
