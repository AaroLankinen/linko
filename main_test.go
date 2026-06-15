package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lmittmann/tint"
)

func Test_requestLogger(t *testing.T) {
	logBuffer := &bytes.Buffer{}

	logger := slog.New(slog.NewTextHandler(logBuffer, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, time.Date(2023, 10, 1, 12, 34, 57, 0, time.UTC))
			}
			if a.Key == "duration" {
				return slog.Duration("duration", 0)
			}
			return replaceAttr(groups, a)
		},
	}))

	requestLoggerMiddleware := requestLogger(logger)
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created successfully"))
	})
	loggedHandler := requestLoggerMiddleware(dummyHandler)

	req := httptest.NewRequest("POST", "http://lin.ko/api/stats", bytes.NewBufferString("hello world!"))
	rr := httptest.NewRecorder()
	loggedHandler.ServeHTTP(rr, req)

	const expectedLogString = `time=2023-10-01T12:34:57.000Z level=INFO msg="Served request" method=POST path=/api/stats client_ip=192.0.2.x duration=0s request_body_bytes=12 response_status=201 response_body_bytes=20` + "\n"
	const expectedStatusCode = http.StatusCreated

	if logBuffer.String() != expectedLogString {
		t.Errorf("Unexpected log string. Got:\n%s\nExpected:\n%s", logBuffer.String(), expectedLogString)
	}

	if rr.Code != expectedStatusCode {
		t.Errorf("Unexpected status code. Got: %d, Expected: %d", rr.Code, expectedStatusCode)
	}
}

func Test_requestLogger_Username(t *testing.T) {
	logBuffer := &bytes.Buffer{}

	logger := slog.New(slog.NewTextHandler(logBuffer, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, time.Date(2023, 10, 1, 12, 34, 57, 0, time.UTC))
			}
			if a.Key == "duration" {
				return slog.Duration("duration", 0)
			}
			return replaceAttr(groups, a)
		},
	}))

	s := &server{logger: logger}

	requestLoggerMiddleware := requestLogger(logger)
	authMiddleware := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	loggedHandler := requestID(requestLoggerMiddleware(authMiddleware))

	req := httptest.NewRequest("GET", "http://lin.ko/api/stats", nil)
	req.SetBasicAuth("frodo", "ofTheNineFingers")
	req.Header.Set("X-Request-ID", "test-request-id-123")
	rr := httptest.NewRecorder()
	loggedHandler.ServeHTTP(rr, req)

	const expectedLogString = `time=2023-10-01T12:34:57.000Z level=INFO msg="Served request" method=GET path=/api/stats client_ip=192.0.2.x duration=0s request_body_bytes=0 response_status=200 response_body_bytes=0 user=[REDACTED] request_id=test-request-id-123` + "\n" +
		`time=2023-10-01T12:34:57.000Z level=INFO msg="Served user" username=frodo` + "\n"

	if logBuffer.String() != expectedLogString {
		t.Errorf("Unexpected log string. Got:\n%s\nExpected:\n%s", logBuffer.String(), expectedLogString)
	}

	if rr.Code != http.StatusOK {
		t.Errorf("Unexpected status code. Got: %d, Expected: %d", rr.Code, http.StatusOK)
	}
}

func Test_ColorizedOutput(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	handler := tint.NewHandler(logBuffer, &tint.Options{
		Level:   slog.LevelInfo,
		NoColor: false,
	})
	logger := slog.New(handler)

	logger.Info("this is a test log message", "key", "val")

	output := logBuffer.String()
	if !strings.Contains(output, "\x1b[") {
		t.Errorf("Expected log output to contain ANSI color escape sequences, got: %q", output)
	}
}

func Test_redactIP(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"192.168.1.50:1234", "192.168.1.x"},
		{"127.0.0.1", "127.0.0.x"},
		{"[::1]:8080", "[::1]:8080"},
		{"[2001:db8::1]:1234", "[2001:db8::1]:1234"},
		{"::1", "::1"},
		{"localhost:8080", "localhost:8080"},
		{"invalid-ip", "invalid-ip"},
		{"", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := redactIP(tc.input)
			if got != tc.expected {
				t.Errorf("redactIP(%q) = %q; want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func Test_replaceAttr_SensitiveAndEmbedded(t *testing.T) {
	tests := []struct {
		key      string
		val      any
		expected any
	}{
		// 1. Sensitive Keys (exact and different cases)
		{"password", "my-secret-password", "[REDACTED]"},
		{"Password", "my-secret-password", "[REDACTED]"},
		{"key", "some-ssh-key", "[REDACTED]"},
		{"KEY", "some-ssh-key", "[REDACTED]"},
		{"apikey", "api-12345", "[REDACTED]"},
		{"secret", "secret-value", "[REDACTED]"},
		{"pin", 1234, "[REDACTED]"},
		{"user", "frodo", "[REDACTED]"},
		{"creditcardno", "1234-5678-9012-3456", "[REDACTED]"},
		// 2. Non-sensitive keys (should not be redacted)
		{"username", "frodo", "frodo"},
		{"keyboard", "mechanical", "mechanical"},
		// 3. Embedded secrets in URL values
		{"connection_url", "postgres://user:password123@localhost:5432/db", "postgres://user:[REDACTED]@localhost:5432/db"},
		{"msg", "failed to connect to mongodb://admin:secretPass!@host:27017/", "failed to connect to mongodb://admin:[REDACTED]@host:27017/"},
		// 4. Embedded secrets in key-value string values
		{"error_message", "db connection failed: password=my-pass user=postgres host=localhost", "db connection failed: password=[REDACTED] user=[REDACTED] host=localhost"},
		{"info", "apikey=xyz&secret=abc", "apikey=[REDACTED]&secret=[REDACTED]"},
		{"trace", "something failed at pin=5555 key=123", "something failed at pin=[REDACTED] key=[REDACTED]"},
	}

	for _, tc := range tests {
		t.Run(tc.key+"_"+fmt.Sprint(tc.val), func(t *testing.T) {
			attr := slog.Any(tc.key, tc.val)
			gotAttr := replaceAttr(nil, attr)
			gotVal := gotAttr.Value.Any()
			if gotVal != tc.expected {
				t.Errorf("replaceAttr(nil, %s=%v) = %v; want %v", tc.key, tc.val, gotVal, tc.expected)
			}
		})
	}
}



