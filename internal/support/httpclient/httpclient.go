package httpclient

import (
	"net/http"
	"net/url"

	"golang.org/x/net/http/httpproxy"
)

func New() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxyConfig := httpproxy.FromEnvironment()
	proxyFunc := proxyConfig.ProxyFunc()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		return proxyFunc(req.URL)
	}

	return &http.Client{Transport: transport}
}
