package httpx

import (
	"net/http"
	"net/url"
	"time"
)

const (
	DefaultTimeout        = 30 * time.Second
	defaultIdleConnTimeout = 90 * time.Second
)

type ClientOptions struct {
	Timeout time.Duration
	// ResponseHeaderTimeout 只约束"请求体发完之后等响应头"的时间，不约束传输本身。
	// 传大文件时应该用它配合 Timeout: -1，而不是给整个请求设总超时。
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	DisableCompression    bool
	DisableKeepAlives     bool
	Proxy                 func(*http.Request) (*url.URL, error)
}

func NewClient(opts ClientOptions) *http.Client {
	// Timeout 为负表示显式要求"不设总超时"——上传/下载这类传输时长取决于文件
	// 大小和链路速度，给它设总超时必然会在慢链路上误杀。0 仍然表示取默认值。
	timeout := opts.Timeout
	switch {
	case timeout < 0:
		timeout = 0
	case timeout == 0:
		timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if opts.ResponseHeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
	}
	idle := opts.IdleConnTimeout
	if idle <= 0 && !opts.DisableKeepAlives {
		idle = defaultIdleConnTimeout
	}
	if idle > 0 {
		tr.IdleConnTimeout = idle
	}
	if opts.DisableCompression {
		tr.DisableCompression = true
	}
	if opts.DisableKeepAlives {
		tr.DisableKeepAlives = true
		tr.MaxIdleConnsPerHost = 0
	}
	if opts.Proxy != nil {
		tr.Proxy = opts.Proxy
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

func CloseClient(c *http.Client) {
	if c == nil {
		return
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		return
	}
	tr.CloseIdleConnections()
}
