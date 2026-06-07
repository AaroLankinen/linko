package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

func main() {
	logger, closeLogger, err := initializeLogger()
	if err != nil {
		slog.Error("failed to initialize logger", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, logger, closeLogger, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, logger *slog.Logger, closeLogger func(), httpPort int, dataDir string) int {
	defer closeLogger()

	st, err := store.New(dataDir)
	if err != nil {
		logger.Info("failed to create store", "error", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	logger.Info("Linko is running", "url", fmt.Sprintf("http://localhost:%d", httpPort))

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	logger.Info("Linko is shutting down")
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Info("failed to shutdown server", "error", err)
		return 1
	}
	if serverErr != nil {
		logger.Info("server error", "error", serverErr)
		return 1
	}
	return 0
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			logger.Info("Served request", "method", r.Method, "path", r.URL.Path)
		})
	}
}

func initializeLogger() (*slog.Logger, func(), error) {
	logFilePath := os.Getenv("LINKO_LOG_FILE")
	if logFilePath == "" {
		handler := slog.NewTextHandler(os.Stderr, nil)
		return slog.New(handler), func() {}, nil
	}

	accessFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, err
	}

	w := bufio.NewWriterSize(io.MultiWriter(os.Stderr, accessFile), 8192)
	handler := slog.NewTextHandler(w, nil)
	logger := slog.New(handler)

	closeFn := func() {
		_ = w.Flush()
		_ = accessFile.Close()
	}

	return logger, closeFn, nil
}
