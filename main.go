package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pkgerr "github.com/pkg/errors"

	"boot.dev/linko/internal"
	"boot.dev/linko/internal/store"
)

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

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
			logger.Info("Served request", "method", r.Method, "path", r.URL.Path, "client_ip", r.RemoteAddr)
		})
	}
}

func initializeLogger() (*slog.Logger, func(), error) {
	logFilePath := os.Getenv("LINKO_LOG_FILE")

	stderrHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	})

	if logFilePath == "" {
		return slog.New(stderrHandler), func() {}, nil
	}

	accessFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, err
	}

	w := bufio.NewWriterSize(accessFile, 8192)
	fileHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: replaceAttr,
	})

	multiHandler := slog.NewMultiHandler(stderrHandler, fileHandler)
	logger := slog.New(multiHandler)

	closeFn := func() {
		_ = w.Flush()
		_ = accessFile.Close()
	}

	return logger, closeFn, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		var attrs []slog.Attr
		attrs = append(attrs, slog.Attr{
			Key:   "message",
			Value: slog.StringValue(err.Error()),
		})
		if stackErr, ok := errors.AsType[stackTracer](err); ok {
			attrs = append(attrs, slog.Attr{
				Key:   "stack_trace",
				Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
			})
		}
		extraAttrs := linkoerr.Attrs(err)
		attrs = append(attrs, extraAttrs...)
		return slog.GroupAttrs("error", attrs...)
	}
	return a
}
