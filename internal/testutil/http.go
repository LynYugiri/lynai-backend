package testutil

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
)

// TestServer 提供不监听端口的 HTTP 测试服务。
//
// 测试仍然可以使用 http.Get、http.Post 或普通 http.Client 访问 URL，
// 请求会被默认 RoundTripper 路由到内存中的 handler，避免受限环境无法
// 创建本地监听端口的问题。
type TestServer struct {
	URL  string
	host string
}

var (
	installTransportOnce sync.Once
	memoryTransport      *inMemoryTransport
	nextServerID         uint64
)

// NewTestServer 注册一个内存 HTTP 服务，并返回可直接拼接路径的 URL。
func NewTestServer(handler http.Handler) *TestServer {
	installInMemoryTransport()

	id := atomic.AddUint64(&nextServerID, 1)
	host := fmt.Sprintf("lynai-test-%d.local", id)
	memoryTransport.register(host, handler)

	return &TestServer{
		URL:  "http://" + host,
		host: host,
	}
}

// NewTestServerFunc 是 http.HandlerFunc 的便捷封装。
func NewTestServerFunc(handler http.HandlerFunc) *TestServer {
	return NewTestServer(handler)
}

// NewHTTPClient 返回显式使用内存 transport 的客户端，适合注入到服务中。
func NewHTTPClient() *http.Client {
	installInMemoryTransport()
	return &http.Client{Transport: memoryTransport}
}

// Close 注销当前服务。已发出的请求不受影响。
func (s *TestServer) Close() {
	if s == nil || s.host == "" {
		return
	}
	memoryTransport.unregister(s.host)
	s.host = ""
}

func installInMemoryTransport() {
	installTransportOnce.Do(func() {
		memoryTransport = &inMemoryTransport{
			fallback: http.DefaultTransport,
			handlers: map[string]http.Handler{},
		}
		http.DefaultTransport = memoryTransport
	})
}

type inMemoryTransport struct {
	fallback http.RoundTripper

	mu       sync.RWMutex
	handlers map[string]http.Handler
}

func (t *inMemoryTransport) register(host string, handler http.Handler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handlers[normalizeHost(host)] = handler
}

func (t *inMemoryTransport) unregister(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.handlers, normalizeHost(host))
}

func (t *inMemoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.RLock()
	handler := t.handlers[normalizeHost(req.URL.Host)]
	t.mu.RUnlock()

	if handler == nil {
		if t.fallback == nil {
			return nil, fmt.Errorf("no handler registered for %s", req.URL.Host)
		}
		return t.fallback.RoundTrip(req)
	}

	return serveInMemory(handler, req), nil
}

func serveInMemory(handler http.Handler, req *http.Request) *http.Response {
	serverReq := req.Clone(req.Context())
	serverURL := *req.URL
	serverURL.Scheme = ""
	serverURL.Host = ""
	serverURL.User = nil
	serverReq.URL = &serverURL
	serverReq.RequestURI = req.URL.RequestURI()
	serverReq.RemoteAddr = "in-memory"
	if serverReq.Host == "" {
		serverReq.Host = req.URL.Host
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, serverReq)

	resp := recorder.Result()
	resp.Request = req
	return resp
}

func normalizeHost(host string) string {
	return strings.ToLower(host)
}

// 确保编译期校验：内存 transport 完整实现 http.RoundTripper。
var _ http.RoundTripper = (*inMemoryTransport)(nil)
