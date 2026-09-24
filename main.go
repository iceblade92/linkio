package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

type closeFunc func() error

func initializeLogger(logfile string) (*slog.Logger, closeFunc, error) {
	w := os.Stderr
	handlers := []slog.Handler{
		tint.NewTextHandler(w, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
			NoColor:     !(isatty.IsTerminal(w.Fd()) || isatty.IsCygwinTerminal(w.Fd())),
		}),
	}
	closers := []closeFunc{}
	if logfile != "" {
		logger := &lumberjack.Logger{
			Filename:   logfile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		close := func() error {
			if err := logger.Close(); err != nil {
				return fmt.Errorf("failed to flush log file: %w", err)
			}
			return nil
		}
		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		}))
		closers = append(closers, close)
	}
	closer := func() error {
		var errs []error
		for _, close := range closers {
			if err := close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

type multiError interface {
	error
	Unwrap() []error
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	var sensitiveKeys = []string{"password", "key", "apikey", "secret", "pin", "creditcardno", "user"}
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindString {
		parsed, err := url.Parse(a.Value.String())
		if err != nil {
			return a
		}
		if parsed.User != nil {
			name := parsed.User.Username()
			_, hasPassword := parsed.User.Password()
			if hasPassword {
				pass := url.UserPassword(name, "[REDACTED]")
				parsed.User = pass
				newurl := parsed.String()
				return slog.String(a.Key, newurl)
			}
		}
	}
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		multi, ok := errors.AsType[multiError](err)
		if ok {
			errs := multi.Unwrap()
			var groups []slog.Attr
			for i, e := range errs {
				attrs := linkoerr.ErrorAttrs(e)
				key := fmt.Sprintf("error_%d", i+1)
				group := slog.GroupAttrs(key, attrs...)
				groups = append(groups, group)
			}
			return slog.GroupAttrs("errors", groups...)
		}

	}
	return a
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open access log: %v", err)
		return 1
	}
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close logger: %v\n", err)
		}
	}()
	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	logger.Debug("Linko is shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}
