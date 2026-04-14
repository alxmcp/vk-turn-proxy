package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cacggghp/vk-turn-proxy/internal/cliutil"
)

func TestParseServerOptionsShowsUsageWithoutArgs(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	_, exitCode := parseServerOptions(nil, "server", &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("parseServerOptions() exitCode = %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output, got %q", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "Usage:\n  server -connect <ip:port> [flags]") {
		t.Fatalf("usage output missing server help text: %q", got)
	}
}

func TestParseServerOptionsShowsHelpFlagUsage(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	_, exitCode := parseServerOptions([]string{"-help"}, "server", &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("parseServerOptions() exitCode = %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output, got %q", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "Examples:") {
		t.Fatalf("expected help examples in output, got %q", got)
	}
}

func TestParseServerOptionsRequiresConnect(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	_, exitCode := parseServerOptions([]string{"-listen", "0.0.0.0:56000"}, "server", &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("parseServerOptions() exitCode = %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected no stdout output, got %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "error: -connect is required") {
		t.Fatalf("expected missing connect error, got %q", got)
	}
}

func TestParseServerOptionsParsesValidArgs(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	opts, exitCode := parseServerOptions([]string{"-connect", "127.0.0.1:51820", "-listen", "0.0.0.0:56000", "-vless", "-debug"}, "server", &stdout, &stderr)
	if exitCode != cliutil.ContinueExecution {
		t.Fatalf("parseServerOptions() exitCode = %d, want %d", exitCode, cliutil.ContinueExecution)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output, got %q", stderr.String())
	}
	if opts.connect != "127.0.0.1:51820" {
		t.Fatalf("connect = %q, want 127.0.0.1:51820", opts.connect)
	}
	if opts.listen != "0.0.0.0:56000" {
		t.Fatalf("listen = %q, want 0.0.0.0:56000", opts.listen)
	}
	if !opts.vlessMode {
		t.Fatal("vlessMode = false, want true")
	}
	if !opts.debug {
		t.Fatal("debug = false, want true")
	}
}

func TestParseServerOptionsParsesTelemostDataChannelArgs(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	opts, exitCode := parseServerOptions([]string{
		"-connect", "127.0.0.1:51820",
		"-yandex-link", "https://telemost.yandex.ru/j/test",
		"-dc",
	}, "server", &stdout, &stderr)
	if exitCode != cliutil.ContinueExecution {
		t.Fatalf("parseServerOptions() exitCode = %d, want %d", exitCode, cliutil.ContinueExecution)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output, got %q", stderr.String())
	}
	if !opts.dc {
		t.Fatal("dc = false, want true")
	}
	if opts.yalink == "" {
		t.Fatal("yalink = empty, want yandex link")
	}
}

func TestParseServerOptionsRejectsDataChannelWithoutRoom(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	_, exitCode := parseServerOptions([]string{
		"-connect", "127.0.0.1:51820",
		"-dc",
	}, "server", &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("parseServerOptions() exitCode = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "-dc requires exactly one of -yandex-link or -jazz-room") {
		t.Fatalf("expected dc validation error, got %q", got)
	}
}

func TestParseServerOptionsAllowsTelemostDataChannelWithVLESS(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	opts, exitCode := parseServerOptions([]string{
		"-connect", "127.0.0.1:51820",
		"-yandex-link", "https://telemost.yandex.ru/j/test",
		"-dc",
		"-vless",
	}, "server", &stdout, &stderr)
	if exitCode != cliutil.ContinueExecution {
		t.Fatalf("parseServerOptions() exitCode = %d, want %d", exitCode, cliutil.ContinueExecution)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output, got %q", stderr.String())
	}
	if !opts.dc || !opts.vlessMode {
		t.Fatalf("expected dc and vlessMode to be true, got dc=%v vlessMode=%v", opts.dc, opts.vlessMode)
	}
}

func TestParseServerOptionsParsesJazzDataChannelArgs(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	opts, exitCode := parseServerOptions([]string{
		"-connect", "127.0.0.1:51820",
		"-jazz-room", "any",
		"-dc",
	}, "server", &stdout, &stderr)
	if exitCode != cliutil.ContinueExecution {
		t.Fatalf("parseServerOptions() exitCode = %d, want %d", exitCode, cliutil.ContinueExecution)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output, got %q", stderr.String())
	}
	if !opts.dc {
		t.Fatal("dc = false, want true")
	}
	if opts.jazzRoom != "any" {
		t.Fatalf("jazzRoom = %q, want any", opts.jazzRoom)
	}
}

func TestParseServerOptionsRejectsJazzDataChannelWithoutRoom(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	_, exitCode := parseServerOptions([]string{
		"-connect", "127.0.0.1:51820",
		"-dc",
	}, "server", &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("parseServerOptions() exitCode = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "-dc requires exactly one of -yandex-link or -jazz-room") {
		t.Fatalf("expected dc validation error, got %q", got)
	}
}

func TestParseServerOptionsRejectsJazzRoomWithoutDataChannel(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	_, exitCode := parseServerOptions([]string{
		"-connect", "127.0.0.1:51820",
		"-jazz-room", "any",
	}, "server", &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("parseServerOptions() exitCode = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "-jazz-room requires -dc") {
		t.Fatalf("expected jazz room validation error, got %q", got)
	}
}
