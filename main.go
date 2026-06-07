package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"bufio"

	"boot.dev/linko/internal/store"
)

func main() {
	logger, closeLogger, err := initializeLogger()
	if err != nil {
		log.Fatalf("failed to initialize logger: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, logger, closeLogger, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, logger *log.Logger, closeLogger func(), httpPort int, dataDir string) int {
	defer closeLogger()

	st, err := store.New(dataDir)
	if err != nil {
		logger.Printf("failed to create store: %v\n", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	logger.Printf("Linko is running on http://localhost:%d\n", httpPort)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	logger.Printf("Linko is shutting down")
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Printf("failed to shutdown server: %v\n", err)
		return 1
	}
	if serverErr != nil {
		logger.Printf("server error: %v\n", serverErr)
		return 1
	}
	return 0
}

func requestLogger(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			logger.Printf("Served request: %s %s\n", r.Method, r.URL.Path)
		})
	}
}

func initializeLogger() (*log.Logger, func(), error) {
	logFilePath := os.Getenv("LINKO_LOG_FILE")
	if logFilePath == "" {
		return log.New(os.Stderr, "", log.LstdFlags), func() {}, nil
	}

	accessFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, err
	}

	w := bufio.NewWriterSize(io.MultiWriter(os.Stderr, accessFile), 8192)
	logger := log.New(w, "", log.LstdFlags)

	closeFn := func() {
		_ = w.Flush()
		_ = accessFile.Close()
	}

	return logger, closeFn, nil
}
