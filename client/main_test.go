package main

import (
	"net/http"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/pion/transport/v4"
)

func TestLocalCaptchaURLForTargetPreservesPathAndQuery(t *testing.T) {
	targetURL, err := neturl.Parse("https://login.vk.ru/captcha/redirect?foo=bar")
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}

	got := localCaptchaURLForTarget(targetURL)
	want := "http://localhost:8765/captcha/redirect?foo=bar"
	if got != want {
		t.Fatalf("localCaptchaURLForTarget() = %q, want %q", got, want)
	}
}

func TestIsLocalCaptchaHostAcceptsLoopbackVariants(t *testing.T) {
	for _, host := range []string{"localhost:8765", "127.0.0.1:8765", "[::1]:8765"} {
		if !isLocalCaptchaHost(host) {
			t.Fatalf("expected %q to be recognized as local captcha host", host)
		}
	}
}

func TestRewriteProxyRequestRewritesLocalHeaders(t *testing.T) {
	targetURL, err := neturl.Parse("https://login.vk.ru/captcha/redirect?step=1")
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://localhost:8765/captcha/check?foo=bar", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "localhost:" + captchaListenPort
	req.Header.Set("Origin", "http://localhost:8765")
	req.Header.Set("Referer", "http://localhost:8765/captcha/redirect?step=1")
	req.Header.Set("Accept-Encoding", "gzip, br")

	rewriteProxyRequest(req, targetURL)

	if req.URL.Scheme != "https" || req.URL.Host != "login.vk.ru" {
		t.Fatalf("rewritten URL host mismatch: %s", req.URL.String())
	}
	if req.URL.Path != "/captcha/check" || req.URL.RawQuery != "foo=bar" {
		t.Fatalf("rewritten URL path mismatch: %s", req.URL.String())
	}
	if got := req.Header.Get("Origin"); got != "https://login.vk.ru" {
		t.Fatalf("Origin = %q, want %q", got, "https://login.vk.ru")
	}
	if got := req.Header.Get("Referer"); got != "https://login.vk.ru/captcha/redirect?step=1" {
		t.Fatalf("Referer = %q, want %q", got, "https://login.vk.ru/captcha/redirect?step=1")
	}
	if got := req.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("Accept-Encoding = %q, want empty", got)
	}
}

func TestExtractSuccessToken(t *testing.T) {
	body := []byte(`{"response":{"success_token":"token-123"}}`)
	if got := extractSuccessToken(body); got != "token-123" {
		t.Fatalf("extractSuccessToken() = %q, want %q", got, "token-123")
	}
}

func TestRewriteProxyCookiesMakesCookiesLocalhostCompatible(t *testing.T) {
	header := http.Header{}
	header.Add("Set-Cookie", "session=abc; Domain=login.vk.ru; Path=/; Secure; HttpOnly; SameSite=None")

	rewriteProxyCookies(header)

	cookies := (&http.Response{Header: header}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 rewritten cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Domain != "" {
		t.Fatalf("cookie Domain = %q, want empty", cookie.Domain)
	}
	if cookie.Secure {
		t.Fatalf("cookie Secure = true, want false")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie SameSite = %v, want %v", cookie.SameSite, http.SameSiteLaxMode)
	}
}

func TestRewriteCaptchaHTMLRewritesTargetOriginAndInjectsScript(t *testing.T) {
	targetURL, err := neturl.Parse("https://login.vk.ru/captcha/redirect?step=1")
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}

	html := `<html><head><script src="https://login.vk.ru/assets/app.js"></script></head><body></body></html>`
	got := rewriteCaptchaHTML(html, targetURL)

	if !strings.Contains(got, "http://localhost:8765/assets/app.js") {
		t.Fatalf("rewritten HTML did not keep assets on local proxy: %s", got)
	}
	if !strings.Contains(got, "MutationObserver") {
		t.Fatalf("rewritten HTML did not inject proxy helper script: %s", got)
	}
}

func TestBrowserOpenCommandsIncludeAndroidHandlers(t *testing.T) {
	commands := browserOpenCommands("android", "http://localhost:8765/captcha")
	if len(commands) < 3 {
		t.Fatalf("android command list too short: %+v", commands)
	}
	if commands[0].name != "termux-open-url" {
		t.Fatalf("first android open command = %q, want %q", commands[0].name, "termux-open-url")
	}

	var hasAM bool
	for _, cmd := range commands {
		if cmd.name == "/system/bin/am" || cmd.name == "am" {
			hasAM = true
			break
		}
	}
	if !hasAM {
		t.Fatalf("android commands do not include Activity Manager fallback: %+v", commands)
	}
}

func TestDirectNetSkipsInterfaceDiscovery(t *testing.T) {
	n := newDirectNet()

	if _, err := n.Interfaces(); err != transport.ErrNotSupported {
		t.Fatalf("Interfaces() error = %v, want %v", err, transport.ErrNotSupported)
	}

	addr, err := n.ResolveUDPAddr("udp", "127.0.0.1:3478")
	if err != nil {
		t.Fatalf("ResolveUDPAddr() error = %v", err)
	}
	if got := addr.String(); got != "127.0.0.1:3478" {
		t.Fatalf("ResolveUDPAddr() = %q, want %q", got, "127.0.0.1:3478")
	}
}
