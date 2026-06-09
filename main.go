package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
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
		logger.Error("failed to create store", "error", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	logger.Debug("Linko is running", "url", fmt.Sprintf("http://localhost:%d", httpPort))

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	logger.Debug("Linko is shutting down")
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server", "error", err)
		return 1
	}
	if serverErr != nil {
		logger.Error("server error", "error", serverErr)
		return 1
	}
	return 0
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			logger.Info(fmt.Sprintf("Served request: %s %s", r.Method, r.URL.Path))
		})
	}
}

func initializeLogger() (*slog.Logger, func(), error) {
	logFilePath := os.Getenv("LINKO_LOG_FILE")

	stderrHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	if logFilePath == "" {
		return slog.New(stderrHandler), func() {}, nil
	}

	accessFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, err
	}

	w := bufio.NewWriterSize(accessFile, 8192)
	fileHandler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})

	multiHandler := slog.NewMultiHandler(stderrHandler, fileHandler)
	logger := slog.New(multiHandler)

	closeFn := func() {
		_ = w.Flush()
		_ = accessFile.Close()
	}

	return logger, closeFn, nil
}
