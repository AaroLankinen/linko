package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	linkoerr "boot.dev/linko/internal"
	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	body := err.Error()
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusInternalServerError {
		body = http.StatusText(status)
	}
	http.Error(w, body, status)
}

type httpInternalError struct {
	internalErr error
	publicMsg   string
}

func (e *httpInternalError) Error() string {
	return e.publicMsg
}

func (e *httpInternalError) StackTrace() pkgerr.StackTrace {
	if tracer, ok := e.internalErr.(stackTracer); ok {
		return tracer.StackTrace()
	}
	return nil
}

func (e *httpInternalError) Unwrap() error {
	return e.internalErr
}

func internalError(internalErr error, publicMsg string) error {
	if publicMsg == "" {
		publicMsg = "Internal Server Error"
	}
	return &httpInternalError{
		internalErr: internalErr,
		publicMsg:   publicMsg,
	}
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

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

func main() {
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger, closeLogger, err := initializeLogger()
	if err != nil {
		slog.Error("failed to initialize logger", "error", err)
		os.Exit(1)
	}

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

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

const requestIDKey contextKey = "request_id"

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = rand.Text()
		}
		w.Header().Set("X-Request-ID", reqID)
		r.Header.Set("X-Request-ID", reqID)
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			sr := &spyReadCloser{ReadCloser: r.Body}
			r.Body = sr
			logCtx := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), logContextKey, logCtx))

			sw := &spyResponseWriter{ResponseWriter: w}

			next.ServeHTTP(sw, r)

			statusCode := sw.statusCode
			if statusCode == 0 {
				statusCode = http.StatusOK
			}

			args := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", r.RemoteAddr,
				slog.Duration("duration", time.Since(start)),
				"request_body_bytes", sr.bytesRead,
				"response_status", statusCode,
				"response_body_bytes", sw.bytesWritten,
			}
			if logCtx.Username != "" {
				args = append(args, "user", logCtx.Username)
			}
			if logCtx.Error != nil {
				args = append(args, "error", logCtx.Error)
			}
			if reqID, ok := r.Context().Value(requestIDKey).(string); ok && reqID != "" {
				args = append(args, "request_id", reqID)
			}

			logger.Info("Served request", args...)

			if logCtx.Username != "" {
				logger.Info("Served user", "username", logCtx.Username)
			}
		})
	}
}

func initializeLogger() (*slog.Logger, func(), error) {
	logFilePath := os.Getenv("LINKO_LOG_FILE")

	var stderrHandler slog.Handler
	if isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()) {
		stderrHandler = tint.NewHandler(os.Stderr, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
		})
	} else {
		stderrHandler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
		})
	}

	if logFilePath == "" {
		return slog.New(stderrHandler), func() {}, nil
	}

	jackLogger := &lumberjack.Logger{
		Filename:   logFilePath,
		MaxSize:    1, // megabytes
		Compress:   true,
	}

	fileHandler := slog.NewJSONHandler(jackLogger, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: replaceAttr,
	})

	multiHandler := slog.NewMultiHandler(stderrHandler, fileHandler)
	logger := slog.New(multiHandler)

	closeFn := func() {
		_ = jackLogger.Close()
	}

	return logger, closeFn, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		if multiErr, ok := errors.AsType[multiError](err); ok {
			var subAttrs []slog.Attr
			for i, subErr := range multiErr.Unwrap() {
				key := fmt.Sprintf("error_%d", i+1)
				subAttrs = append(subAttrs, slog.GroupAttrs(key, errorToAttrs(subErr)...))
			}
			return slog.GroupAttrs("errors", subAttrs...)
		}
		return slog.GroupAttrs("error", errorToAttrs(err)...)
	}
	return a
}

func errorToAttrs(err error) []slog.Attr {
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
	return attrs
}
