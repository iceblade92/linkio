package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const shortURLLen = len("http://localhost:8080/") + 6

var (
	redirectsMu sync.Mutex
	redirects   []string
)

//go:embed index.html
var indexPage string

func (s *server) handlerIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, indexPage)
}

func (s *server) handlerLogin(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) handlerShortenLink(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(UserContextKey).(string)
	if !ok || user == "" {
		httpError(r.Context(), w, http.StatusUnauthorized, errors.New("unauthorized"))
		return
	}
	longURL := r.FormValue("url")
	if longURL == "" {
		httpError(r.Context(), w, http.StatusBadRequest, errors.New("missing url parameter"))
		return
	}
	u, err := url.Parse(longURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		httpError(r.Context(), w, http.StatusBadRequest, errors.New("invalid URL: must include scheme (http/https) and host"))
		return
	}
	if err := checkDestination(longURL); err != nil {
		httpError(r.Context(), w, http.StatusBadRequest, fmt.Errorf("invalid target URL: %w", err))
		return
	}
	shortCode, err := s.store.Create(r.Context(), longURL)
	if err != nil {
		httpError(r.Context(), w, http.StatusInternalServerError, fmt.Errorf("failed to shorten URL: %w", err))
		return
	}
	s.logger.Info("Successfully generated short code", "method", r.Method, "path", r.URL.Path, "client_ip", "http://localhost:8080/", "scheme", u.Scheme, "host", u.Host, "short", shortCode, "long", longURL)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, shortCode)
}

func (s *server) handlerRedirect(w http.ResponseWriter, r *http.Request) {
	longURL, err := s.store.Lookup(r.Context(), r.PathValue("shortCode"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(r.Context(), w, http.StatusNotFound, errors.New("not found"))
		} else {
			s.logger.Error("failed to lookup URL", "error", err)
			httpError(r.Context(), w, http.StatusInternalServerError, fmt.Errorf("internal server error: %w", err))
		}
		return
	}
	_, _ = bcrypt.GenerateFromPassword([]byte(longURL), bcrypt.DefaultCost)
	if err := checkDestination(longURL); err != nil {
		httpError(r.Context(), w, http.StatusBadGateway, errors.New("destination unavailable"))
		return
	}

	redirectsMu.Lock()
	redirects = append(redirects, strings.Repeat(longURL, 1024))
	redirectsMu.Unlock()

	http.Redirect(w, r, longURL, http.StatusFound)
}

func (s *server) handlerListURLs(w http.ResponseWriter, r *http.Request) {
	codes, err := s.store.List(r.Context())
	if err != nil {
		httpError(r.Context(), w, http.StatusInternalServerError, fmt.Errorf("failed to list URLs: %w", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(codes)
}

func (s *server) handlerStats(w http.ResponseWriter, _ *http.Request) {
	redirectsMu.Lock()
	snapshot := redirects
	redirectsMu.Unlock()

	var bytesSaved int
	for _, u := range snapshot {
		bytesSaved += len(u) - shortURLLen
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{
		"redirects":   len(snapshot),
		"bytes_saved": bytesSaved,
	})
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	message := err.Error()
	switch status {
	case 401:
		message = http.StatusText(status)
	case 403:
		message = http.StatusText(status)
	case 500:
		message = http.StatusText(status)
	}
	http.Error(w, message, status)
}

func redactIP(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	parsedip := net.ParseIP(host)
	ipv4 := parsedip.To4()
	if ipv4 == nil {
		return address
	}
	redacted := fmt.Sprintf("%d.%d.%d.x", ipv4[0], ipv4[1], ipv4[2])
	return redacted
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			spyWriter := &spyResponseWriter{ResponseWriter: w}
			spyReader := &spyReadCloser{ReadCloser: r.Body}
			r.Body = spyReader
			logCtx := &LogContext{}
			ctx := context.WithValue(r.Context(), logContextKey, logCtx)
			r = r.WithContext(ctx)
			next.ServeHTTP(spyWriter, r)
			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", redactIP(r.RemoteAddr)),
				slog.Duration("duration", time.Since(start)),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.String("request_id", spyWriter.Header().Get("X-Request-ID")),
			}
			if logCtx.Username != "" {
				attrs = append(attrs, slog.String("user", logCtx.Username))
			}
			if logCtx.Error != nil {
				groupValue := slog.GroupValue(linkoerr.ErrorAttrs(logCtx.Error)...)
				attrs = append(attrs, slog.Attr{
					Key:   "error",
					Value: groupValue,
				})
			}
			logger.Info("Served request", attrs...)
		})
	}
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
