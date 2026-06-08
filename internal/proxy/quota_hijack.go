// quota_hijack.go — SSE 流的配额监控与连接劫持。
//
// 设计要点（docs/quota-design.md §9.3 / §15）：
//   - Hijack 仅在检测到 SSE 响应（text/event-stream）时执行；非 SSE 请求走原路径。
//   - Hijack 失败（ResponseWriter 未实现 http.Hijacker）降级为原 io.Copy 路径，记录 WARN 日志，不阻断请求。
//   - monitorActiveStream 向 quota.Manager 注册 cancel 函数；Manager 评估超限时调用 cancel，
//     触发 conn.SetLinger(0) + conn.Close() → TCP RST，中断 SSE 长连接。
//   - cancel 闭包保证只执行一次（atomic CAS），避免对已关闭 channel 的二次 close（防 panic）。
//   - 外层监控 goroutine 通过 stopCh 退出，cancelOnce 触发，杜绝 goroutine 泄漏。
package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
)

// errHijackNotSupported 标识 ResponseWriter 不支持 Hijacker 接口。
var errHijackNotSupported = errors.New("quota: ResponseWriter does not implement http.Hijacker")

// hijackForQuotaMonitor 尝试劫持客户端连接并返回底层 conn，供配额监控使用。
// 调用方必须在 Hijack 前写完所有响应 headers 并 Flush。
// 若 ResponseWriter 未实现 http.Hijacker（如 httptest.ResponseRecorder），
// 返回 errHijackNotSupported，调用方应降级到原 io.Copy 路径（§15 风险缓解）。
func hijackForQuotaMonitor(w http.ResponseWriter) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errHijackNotSupported
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// monitorActiveStream 为 SSE 流注册配额监控，确保 Manager 触发超限时能通过 TCP RST 中断流。
//
// 行为：
//   - quotaManager == nil 或 conn == nil 时返回 no-op 闭包，不启动 goroutine。
//   - 否则向 quotaManager 注册 cancel 函数：Manager 超限时调用 cancel → SetLinger(0) + Close → RST。
//   - 启动外层 goroutine 监听 ctx.Done()：客户端断连时执行 conn.Close()（FIN，不 RST）。
//   - 返回闭包：调用 unregister + cancelOnce（关闭 stopCh 让 goroutine 自然退出）。
//
// 调用方必须在流结束时 defer 调用返回的闭包，否则 goroutine 会泄漏。
func (s *Service) monitorActiveStream(ctx context.Context, clientName string, conn net.Conn) (cancel func()) {
	if s.quotaManager == nil || conn == nil {
		return func() {}
	}

	// mgrCancel：Manager 超限时调用，触发 TCP RST 中断 SSE 流。
	mgrCancel := func() {
		// SetLinger 仅在 TCPConn 上可用（HTTP Hijack 返回 *net.TCPConn）。
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
	unregister := s.quotaManager.RegisterActiveStream(clientName, mgrCancel)

	// stopCh：用于通知监控 goroutine 主流程已收尾，可以退出。
	stopCh := make(chan struct{})
	var closed int32
	cancelOnce := func() {
		if atomic.CompareAndSwapInt32(&closed, 0, 1) {
			close(stopCh)
		}
	}

	go func() {
		select {
		case <-ctx.Done():
			// 客户端断连或服务端主动取消：FIN（不 RST）
			_ = conn.Close()
		case <-stopCh:
			// 主流程收尾：不主动 close conn（io.Copy 已返回，conn 由上层管理）
		}
	}()

	return func() {
		unregister()
		cancelOnce()
	}
}
