package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	linkoerr "boot.dev/linko/internal"
	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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
	defer cancel()

	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		logger.Error("failed to initialize tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			logger.Error("failed to shutdown tracing", "error", err)
		}
	}()

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, logger, closeLogger, *httpPort, *dataDir)
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
				"client_ip", redactIP(r.RemoteAddr),
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

func initTracing(ctx context.Context) (func(context.Context) error, error) {
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			"",
			attribute.String("service.name", "linko"),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
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
		Filename: logFilePath,
		MaxSize:  1, // megabytes
		Compress: true,
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

var sensitiveKeys = map[string]bool{
	"password":     true,
	"key":          true,
	"apikey":       true,
	"secret":       true,
	"pin":          true,
	"user":         true,
	"creditcardno": true,
}

// httpRequestsTotal counts requests by method, path and status.
var httpRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	},
	[]string{"method", "path", "status"},
)

// urlPasswordRegex matches passwords in URLs.
var urlPasswordRegex = regexp.MustCompile(`(?i)([a-z0-9+.-]+://[^/:]+:)([^@/]+)(@)`)

// kvPasswordRegex matches passwords in key-value pairs.
var kvPasswordRegex = regexp.MustCompile(`(?i)\b(password|key|apikey|secret|pin|user|creditcardno)\b\s*=\s*([^&?\s"';]+)`)

func isSensitiveKey(key string) bool {
	return sensitiveKeys[strings.ToLower(key)]
}

func redactEmbeddedSecrets(val string) string {
	val = urlPasswordRegex.ReplaceAllString(val, "${1}[REDACTED]${3}")
	val = kvPasswordRegex.ReplaceAllString(val, "${1}=[REDACTED]")
	return val
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		path := r.URL.Path
		method := r.Method
		status := strconv.Itoa(rec.status)

		httpRequestsTotal.
			WithLabelValues(method, path, status).
			Inc()
	})
}

func sanitizeAttr(a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) && a.Value.Kind() != slog.KindGroup {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindString {
		str := a.Value.String()
		redacted := redactEmbeddedSecrets(str)
		if redacted != str {
			return slog.String(a.Key, redacted)
		}
	} else if a.Value.Kind() == slog.KindAny {
		if str, ok := a.Value.Any().(string); ok {
			redacted := redactEmbeddedSecrets(str)
			if redacted != str {
				return slog.String(a.Key, redacted)
			}
		}
	}
	return a
}

func sanitizeAttrs(attrs []slog.Attr) []slog.Attr {
	sanitized := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		sanitized[i] = sanitizeAttr(a)
	}
	return sanitized
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return sanitizeAttr(a)
		}
		if multiErr, ok := errors.AsType[multiError](err); ok {
			var subAttrs []slog.Attr
			for i, subErr := range multiErr.Unwrap() {
				key := fmt.Sprintf("error_%d", i+1)
				subAttrs = append(subAttrs, slog.GroupAttrs(key, sanitizeAttrs(errorToAttrs(subErr))...))
			}
			return slog.GroupAttrs("errors", subAttrs...)
		}
		return slog.GroupAttrs("error", sanitizeAttrs(errorToAttrs(err))...)
	}
	return sanitizeAttr(a)
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

func redactIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// If there is no port, check if it's a raw IPv4 address
		ip := net.ParseIP(addr)
		if ip != nil && ip.To4() != nil {
			parts := strings.Split(addr, ".")
			if len(parts) == 4 {
				parts[3] = "x"
				return strings.Join(parts, ".")
			}
		}
		return addr
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.To4() != nil {
		parts := strings.Split(host, ".")
		if len(parts) == 4 {
			parts[3] = "x"
			return strings.Join(parts, ".")
		}
	}
	return addr
}
