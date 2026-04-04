package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/proxy"
)

// HttpProxy represents an HTTP proxy server that forwards through SOCKS5
type HttpProxy struct {
	listener  net.Listener
	socksAddr string // Address of the SOCKS5 proxy (e.g., "127.0.0.1:1080")
	httpAddr  string // Address to listen for HTTP requests (e.g., "127.0.0.1:8080")
	stopChan  chan struct{}
	wg        sync.WaitGroup
	isRunning bool
	mu        sync.Mutex
}

// NewHttpProxy creates a new HTTP proxy server
func NewHttpProxy(httpAddr, socksAddr string) *HttpProxy {
	return &HttpProxy{
		socksAddr: socksAddr,
		httpAddr:  httpAddr,
		stopChan:  make(chan struct{}),
	}
}

// Start begins listening for HTTP connections
func (s *HttpProxy) Start() error {
	s.mu.Lock()
	if s.isRunning {
		s.mu.Unlock()
		return fmt.Errorf("HTTP proxy already running")
	}
	s.mu.Unlock()

	listener, err := net.Listen("tcp", s.httpAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.httpAddr, err)
	}

	s.listener = listener
	s.mu.Lock()
	s.isRunning = true
	s.mu.Unlock()

	log.Printf("[HTTP Proxy] Starting on %s (forwarding to SOCKS5 %s)", s.httpAddr, s.socksAddr)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				select {
				case <-s.stopChan:
					return
				default:
					log.Printf("[HTTP Proxy] Accept error: %v", err)
					continue
				}
			}
			s.wg.Add(1)
			go func(c net.Conn) {
				defer s.wg.Done()
				s.handleConnection(c)
			}(conn)
		}
	}()

	return nil
}

// Stop gracefully shuts down the HTTP proxy server
func (s *HttpProxy) Stop() {
	s.mu.Lock()
	if !s.isRunning {
		s.mu.Unlock()
		return
	}
	s.isRunning = false
	s.mu.Unlock()

	close(s.stopChan)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
	log.Printf("[HTTP Proxy] Stopped")
}

// handleConnection processes incoming HTTP connections
func (s *HttpProxy) handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	reader := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("[HTTP Proxy] Error reading request: %v", err)
		return
	}

	if req.Method == http.MethodConnect {
		s.handleHTTPS(clientConn, req)
	} else {
		s.handleHTTP(clientConn, req)
	}
}

// handleHTTPS handles HTTPS requests using HTTP CONNECT method
func (s *HttpProxy) handleHTTPS(clientConn net.Conn, req *http.Request) {
	destAddr := req.Host

	// Connect to SOCKS5 proxy
	socksDialer, err := proxy.SOCKS5("tcp", s.socksAddr, nil, proxy.Direct)
	if err != nil {
		log.Printf("[HTTP Proxy] SOCKS5 dialer error: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Connect to destination through SOCKS5
	targetConn, err := socksDialer.Dial("tcp", destAddr)
	if err != nil {
		log.Printf("[HTTP Proxy] Target connection error: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// Tell client connection is established
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional copy
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(targetConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, targetConn)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-s.stopChan:
	}
}

// handleHTTP handles plain HTTP requests (GET, POST, etc.)
func (s *HttpProxy) handleHTTP(clientConn net.Conn, req *http.Request) {
	defer clientConn.Close()

	// Extract destination from URL
	destAddr := req.URL.Host
	if destAddr == "" {
		// If URL doesn't have host, try to parse from full URL
		if !strings.HasPrefix(req.URL.String(), "http") {
			req.URL.Host = req.Host
			req.URL.Scheme = "http"
		}
		destAddr = req.URL.Host
	}

	if destAddr == "" {
		clientConn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 26\r\n\r\nBad Request: No destination host"))
		return
	}

	// Connect to SOCKS5 proxy
	socksDialer, err := proxy.SOCKS5("tcp", s.socksAddr, nil, proxy.Direct)
	if err != nil {
		log.Printf("[HTTP Proxy] SOCKS5 dialer error: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 13\r\n\r\n502 Bad Gateway"))
		return
	}

	// Connect to destination through SOCKS5
	targetConn, err := socksDialer.Dial("tcp", destAddr)
	if err != nil {
		log.Printf("[HTTP Proxy] Target connection error: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 13\r\n\r\n502 Bad Gateway"))
		return
	}
	defer targetConn.Close()

	// Rewrite request to be proper HTTP/1.1
	req.URL.Scheme = "http"
	req.RequestURI = ""
	req.Host = destAddr

	// Forward request
	err = req.Write(targetConn)
	if err != nil {
		log.Printf("[HTTP Proxy] Error forwarding request: %v", err)
		return
	}

	// Read response from target
	reader := bufio.NewReader(targetConn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		log.Printf("[HTTP Proxy] Error reading response: %v", err)
		return
	}

	// Forward response to client
	err = resp.Write(clientConn)
	if err != nil {
		log.Printf("[HTTP Proxy] Error writing response: %v", err)
		return
	}
}
