// Copyright 2026 The Outband Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type bufferPool struct {
	pool sync.Pool
}

func (b *bufferPool) Get() []byte {
	if v := b.pool.Get(); v != nil {
		return v.([]byte)
	}
	return make([]byte, 32*1024)
}

func (b *bufferPool) Put(buf []byte) {
	b.pool.Put(buf)
}

func newProxy(targetURL *url.URL) *httputil.ReverseProxy {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(targetURL)
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			// Go's Request.write() injects "User-Agent: Go-http-client/1.1"
			// whenever the header is absent. To preserve transparency when
			// the client sent no User-Agent, set an explicitly empty value
			// which suppresses the default without sending a header on the wire.
			if _, ok := r.In.Header["User-Agent"]; !ok {
				r.Out.Header["User-Agent"] = []string{""}
			}
		},
		Transport:  transport,
		BufferPool: &bufferPool{},
	}
}

func main() {
	target := flag.String("target", "", "upstream API URL (e.g., https://api.openai.com)")
	listen := flag.String("listen", "localhost:8080", "address to listen on")
	flag.Parse()

	if *target == "" {
		fmt.Fprintln(os.Stderr, "error: --target is required")
		flag.Usage()
		os.Exit(1)
	}

	targetURL, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("invalid target URL: %v", err)
	}

	if targetURL.Scheme != "http" && targetURL.Scheme != "https" {
		log.Fatalf("target URL must have http or https scheme, got %q", targetURL.Scheme)
	}

	proxy := newProxy(targetURL)

	server := &http.Server{
		Addr:    *listen,
		Handler: proxy,
	}

	log.Printf("proxying %s -> %s", *listen, targetURL)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
}
