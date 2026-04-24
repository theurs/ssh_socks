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
	"time"

	"golang.org/x/net/proxy"
)

// HTTPProxy represents an HTTP proxy server that forwards through SOCKS5
type HTTPProxy struct {
	listener  net.Listener
	socksAddr string // Address of the SOCKS5 proxy (e.g., "127.0.0.1:1080")
	httpAddr  string // Address to listen for HTTP requests (e.g., "127.0.0.1:8080")
	stopChan  chan struct{}
	wg        sync.WaitGroup
	isRunning bool
	mu        sync.Mutex
}

// NewHTTPProxy creates a new HTTP proxy server
func NewHTTPProxy(httpAddr, socksAddr string) *HTTPProxy {
	return &HTTPProxy{
		socksAddr: socksAddr,
		httpAddr:  httpAddr,
		stopChan:  make(chan struct{}),
	}
}

// Start begins listening for HTTP connections
func (s *HTTPProxy) Start() error {
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
func (s *HTTPProxy) Stop() {
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
func (s *HTTPProxy) handleConnection(clientConn net.Conn) {
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

func (s *HTTPProxy) handleHTTPS(clientConn net.Conn, req *http.Request) {
	destAddr := req.Host

	var targetConn net.Conn
	var err error

	// Проверяем, нужно ли фильтровать этот адрес
	shouldUseProxy, reason := shouldProxy(destAddr)

	if shouldUseProxy {
		// Логируем в отдельный файл
		proxyLog("[HTTP-S] PROXY | %-30s | Reason: %s", destAddr, reason)

		// Соединяемся через SOCKS5 туннель (на порт 1080 или 1081)
		socksDialer, errDialer := proxy.SOCKS5("tcp", s.socksAddr, nil, proxy.Direct)
		if errDialer != nil {
			log.Printf("[HTTP Proxy] SOCKS5 dialer error: %v", errDialer)
			clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
			return
		}
		targetConn, err = socksDialer.Dial("tcp", destAddr)
	} else {
		// Соединяемся напрямую
		targetConn, err = net.DialTimeout("tcp", destAddr, 10*time.Second)
	}

	if err != nil {
		log.Printf("[HTTP Proxy] Target connection error to %s: %v", destAddr, err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// Подтверждаем установку туннеля клиенту
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		return
	}

	// Копируем данные в обе стороны с защитой от зомби-горутин
	relay := func(dst, src net.Conn) {
		io.Copy(dst, src)
		dst.SetDeadline(time.Now())
		src.SetDeadline(time.Now())
	}

	go relay(targetConn, clientConn)
	relay(clientConn, targetConn)
}

func (s *HTTPProxy) handleHTTP(clientConn net.Conn, req *http.Request) {
	defer clientConn.Close()

	destAddr := req.URL.Host
	if destAddr == "" {
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

	var targetConn net.Conn
	var err error

	// Решаем: туннель или прямой выход
	shouldUseProxy, reason := shouldProxy(destAddr)

	if shouldUseProxy {
		// Логируем проксирование
		proxyLog("[HTTP  ] PROXY | %-30s | Reason: %s", destAddr, reason)

		socksDialer, errDialer := proxy.SOCKS5("tcp", s.socksAddr, nil, proxy.Direct)
		if errDialer != nil {
			log.Printf("[HTTP Proxy] SOCKS5 dialer error: %v", errDialer)
			clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 13\r\n\r\n502 Bad Gateway"))
			return
		}
		targetConn, err = socksDialer.Dial("tcp", destAddr)
	} else {
		// Прямое соединение
		targetConn, err = net.DialTimeout("tcp", destAddr, 10*time.Second)
	}

	if err != nil {
		log.Printf("[HTTP Proxy] Target connection error to %s: %v", destAddr, err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 13\r\n\r\n502 Bad Gateway"))
		return
	}
	defer targetConn.Close()

	// Переписываем запрос для передачи серверу
	req.URL.Scheme = "http"
	req.RequestURI = ""
	req.Host = destAddr

	err = req.Write(targetConn)
	if err != nil {
		log.Printf("[HTTP Proxy] Error forwarding request: %v", err)
		return
	}

	// Читаем и пересылаем ответ
	reader := bufio.NewReader(targetConn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		log.Printf("[HTTP Proxy] Error reading response: %v", err)
		return
	}

	err = resp.Write(clientConn)
	if err != nil {
		log.Printf("[HTTP Proxy] Error writing response: %v", err)
		return
	}

	// Принудительное завершение для предотвращения висячих Keep-Alive горутин
	if resp.Close || req.Close {
		targetConn.SetDeadline(time.Now())
		clientConn.SetDeadline(time.Now())
	}
}
