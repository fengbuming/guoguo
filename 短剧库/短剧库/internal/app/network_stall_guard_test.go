package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// 对端连接建立后完全不写数据（模拟 CDN 静默挂起 / TCP 半开），
// Read 必须在 stall 超时后返回 i/o timeout，而不是永久阻塞。
func TestStallGuardConnReadTimesOutWhenPeerSilent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	release := make(chan struct{})
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			<-release
			connection.Close()
		}
	}()
	defer close(release)

	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	connection := &stallGuardConn{Conn: raw, timeout: 150 * time.Millisecond}

	started := time.Now()
	buffer := make([]byte, 16)
	if _, err := connection.Read(buffer); err == nil {
		t.Fatal("expected read timeout, got nil error")
	} else if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("read blocked too long: %v", elapsed)
	}
}

// 对端持续慢速写数据时，deadline 每次 Read 顺延，不得误触发超时。
func TestStallGuardConnReadRefreshesDeadlineWhileDataFlows(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		for index := 0; index < 5; index++ {
			time.Sleep(60 * time.Millisecond)
			if _, err := connection.Write([]byte("x")); err != nil {
				return
			}
		}
	}()

	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	connection := &stallGuardConn{Conn: raw, timeout: 200 * time.Millisecond}

	for index := 0; index < 5; index++ {
		buffer := make([]byte, 1)
		if _, err := connection.Read(buffer); err != nil {
			t.Fatalf("read %d: unexpected error %v", index, err)
		}
	}
}

// DialContext 拨出的连接（非保护 CDN 的普通路径）必须包 stallGuardConn，
// 这样播放链路读超时才会触发媒体续读。
func TestSafeDNSDialerWrapsConnectionsWithStallGuard(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()

	resolver := newSafeDNSDialer(http.DefaultTransport.(*http.Transport).Clone())
	connection, err := resolver.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()
	guard, ok := connection.(*stallGuardConn)
	if !ok {
		t.Fatalf("expected *stallGuardConn, got %T", connection)
	}
	if guard.timeout != connReadStallTimeout {
		t.Fatalf("guard timeout = %v, want %v", guard.timeout, connReadStallTimeout)
	}
}
