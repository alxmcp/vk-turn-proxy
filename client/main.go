// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bschaatsbergen/dnsdialer"
	"github.com/cacggghp/vk-turn-proxy/tcputil"
	"github.com/cbeuw/connutil"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/transport/v4"
	"github.com/pion/turn/v5"
	"github.com/xtaci/smux"
)

type getCredsFunc func(string) (string, string, string, error)

const captchaListenPort = "8765"

type browserCommand struct {
	name string
	args []string
}

type directNet struct{}

type directDialer struct {
	*net.Dialer
}

type directListenConfig struct {
	*net.ListenConfig
}

func newDirectNet() transport.Net {
	return directNet{}
}

func (directNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	return net.ListenPacket(network, address) //nolint:noctx
}

func (directNet) ListenUDP(network string, locAddr *net.UDPAddr) (transport.UDPConn, error) {
	return net.ListenUDP(network, locAddr)
}

func (directNet) ListenTCP(network string, laddr *net.TCPAddr) (transport.TCPListener, error) {
	listener, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	return directTCPListener{listener}, nil
}

func (directNet) Dial(network, address string) (net.Conn, error) {
	return net.Dial(network, address) //nolint:noctx
}

func (directNet) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	return net.DialUDP(network, laddr, raddr)
}

func (directNet) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	return net.DialTCP(network, laddr, raddr)
}

func (directNet) ResolveIPAddr(network, address string) (*net.IPAddr, error) {
	return net.ResolveIPAddr(network, address)
}

func (directNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr(network, address)
}

func (directNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

func (directNet) Interfaces() ([]*transport.Interface, error) {
	return nil, transport.ErrNotSupported
}

func (directNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	return nil, fmt.Errorf("%w: index=%d", transport.ErrInterfaceNotFound, index)
}

func (directNet) InterfaceByName(name string) (*transport.Interface, error) {
	return nil, fmt.Errorf("%w: %s", transport.ErrInterfaceNotFound, name)
}

func (directNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	return directDialer{Dialer: dialer}
}

func (directNet) CreateListenConfig(listenerConfig *net.ListenConfig) transport.ListenConfig {
	return directListenConfig{ListenConfig: listenerConfig}
}

func (d directDialer) Dial(network, address string) (net.Conn, error) {
	return d.Dialer.Dial(network, address)
}

func (d directListenConfig) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	return d.ListenConfig.Listen(ctx, network, address)
}

func (d directListenConfig) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	return d.ListenConfig.ListenPacket(ctx, network, address)
}

type directTCPListener struct {
	*net.TCPListener
}

func (l directTCPListener) AcceptTCP() (transport.TCPConn, error) {
	return l.TCPListener.AcceptTCP()
}

func localCaptchaOrigin() string {
	return "http://localhost:" + captchaListenPort
}

func localCaptchaListenAddrs() []string {
	return []string{
		"127.0.0.1:" + captchaListenPort,
		"[::1]:" + captchaListenPort,
	}
}

func localCaptchaHosts() []string {
	return []string{
		"localhost:" + captchaListenPort,
		"127.0.0.1:" + captchaListenPort,
		"[::1]:" + captchaListenPort,
	}
}

func isLocalCaptchaHost(host string) bool {
	for _, localHost := range localCaptchaHosts() {
		if strings.EqualFold(host, localHost) {
			return true
		}
	}

	return false
}

func localCaptchaURLForTarget(targetURL *neturl.URL) string {
	localURL := &neturl.URL{
		Scheme:   "http",
		Host:     "localhost:" + captchaListenPort,
		Path:     targetURL.Path,
		RawPath:  targetURL.RawPath,
		RawQuery: targetURL.RawQuery,
	}
	if localURL.Path == "" {
		localURL.Path = "/"
	}

	return localURL.String()
}

func targetOrigin(targetURL *neturl.URL) string {
	return targetURL.Scheme + "://" + targetURL.Host
}

func rewriteProxyHeaderURL(raw string, targetURL *neturl.URL) string {
	if raw == "" {
		return raw
	}

	parsed, err := neturl.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.Scheme != "http" || !isLocalCaptchaHost(parsed.Host) {
		return raw
	}

	parsed.Scheme = targetURL.Scheme
	parsed.Host = targetURL.Host

	return parsed.String()
}

func rewriteProxyRequest(req *http.Request, targetURL *neturl.URL) {
	req.URL.Scheme = targetURL.Scheme
	req.URL.Host = targetURL.Host
	if req.URL.Path == "" {
		req.URL.Path = targetURL.Path
	}
	req.Host = targetURL.Host

	req.Header.Del("Accept-Encoding")
	for _, headerName := range []string{"Origin", "Referer"} {
		if rewritten := rewriteProxyHeaderURL(req.Header.Get(headerName), targetURL); rewritten != "" {
			req.Header.Set(headerName, rewritten)
		} else {
			req.Header.Del(headerName)
		}
	}
}

func extractSuccessToken(body []byte) string {
	var payload struct {
		Response struct {
			SuccessToken string `json:"success_token"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	return payload.Response.SuccessToken
}

func rewriteProxyCookies(header http.Header) {
	cookies := (&http.Response{Header: header}).Cookies()
	if len(cookies) == 0 {
		return
	}

	header.Del("Set-Cookie")
	for _, cookie := range cookies {
		cookie.Domain = ""
		cookie.Secure = false
		cookie.Partitioned = false
		if cookie.SameSite == http.SameSiteNoneMode || cookie.SameSite == http.SameSiteStrictMode {
			cookie.SameSite = http.SameSiteLaxMode
		}
		header.Add("Set-Cookie", cookie.String())
	}
}

func rewriteCaptchaHTML(html string, targetURL *neturl.URL) string {
	localOrigin := localCaptchaOrigin()
	upstreamOrigin := targetOrigin(targetURL)
	html = strings.ReplaceAll(html, upstreamOrigin, localOrigin)

	script := fmt.Sprintf(`
<script>
(function() {
    var localOrigin = %q;
    var upstreamOrigin = %q;

    function rewriteUrl(urlStr) {
        if (!urlStr || typeof urlStr !== 'string') return urlStr;
        if (urlStr.indexOf(localOrigin) === 0) return urlStr;
        if (urlStr.indexOf(upstreamOrigin) === 0) return localOrigin + urlStr.slice(upstreamOrigin.length);
        if (urlStr.indexOf('//') === 0) {
            return '/generic_proxy?proxy_url=' + encodeURIComponent(window.location.protocol + urlStr);
        }
        if (urlStr.indexOf('http://') === 0 || urlStr.indexOf('https://') === 0) {
            return '/generic_proxy?proxy_url=' + encodeURIComponent(urlStr);
        }
        return urlStr;
    }

    function rewriteElementAttr(el, attr) {
        if (!el || !el.getAttribute) return;
        var value = el.getAttribute(attr);
        if (!value) return;
        var rewritten = rewriteUrl(value);
        if (rewritten !== value) {
            el.setAttribute(attr, rewritten);
        }
    }

    function rewriteDocument(root) {
        if (!root || !root.querySelectorAll) return;
        root.querySelectorAll('[href]').forEach(function(el) { rewriteElementAttr(el, 'href'); });
        root.querySelectorAll('[src]').forEach(function(el) { rewriteElementAttr(el, 'src'); });
        root.querySelectorAll('form[action]').forEach(function(el) { rewriteElementAttr(el, 'action'); });
    }

    function handleSuccessToken(token) {
        if (!token) return;
        fetch('/local-captcha-result', {
            method: 'POST',
            headers: {'Content-Type': 'application/x-www-form-urlencoded'},
            body: 'token=' + encodeURIComponent(token)
        }).then(function() {
            document.body.innerHTML = '<h2 style="text-align:center;margin-top:20vh">Готово! Можете закрыть страницу.</h2>';
            setTimeout(function() { window.close(); }, 300);
        }).catch(function() {});
    }

    var origOpen = XMLHttpRequest.prototype.open;
    XMLHttpRequest.prototype.open = function() {
        if (arguments[1] && typeof arguments[1] === 'string') {
            this._origUrl = arguments[1];
            arguments[1] = rewriteUrl(arguments[1]);
        }
        return origOpen.apply(this, arguments);
    };

    var origSend = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.send = function() {
        var xhr = this;
        if (this._origUrl && this._origUrl.indexOf('captchaNotRobot.check') !== -1) {
            xhr.addEventListener('load', function() {
                try {
                    var data = JSON.parse(xhr.responseText);
                    if (data.response && data.response.success_token) {
                        handleSuccessToken(data.response.success_token);
                    }
                } catch (e) {}
            });
        }
        return origSend.apply(this, arguments);
    };

    var origFetch = window.fetch;
    if (origFetch) {
        window.fetch = function() {
            var url = arguments[0];
            var isObj = (typeof url === 'object' && url && url.url);
            var urlStr = isObj ? url.url : url;
            var origUrlStr = urlStr;

            if (typeof urlStr === 'string') {
                urlStr = rewriteUrl(urlStr);
                arguments[0] = urlStr;
            }

            var p = origFetch.apply(this, arguments);
            if (typeof origUrlStr === 'string' && origUrlStr.indexOf('captchaNotRobot.check') !== -1) {
                p.then(function(response) {
                    return response.clone().json();
                }).then(function(data) {
                    if (data.response && data.response.success_token) {
                        handleSuccessToken(data.response.success_token);
                    }
                }).catch(function() {});
            }
            return p;
        };
    }

    document.addEventListener('submit', function(event) {
        if (event.target && event.target.action) {
            event.target.action = rewriteUrl(event.target.action);
        }
    }, true);

    document.addEventListener('click', function(event) {
        var target = event.target && event.target.closest ? event.target.closest('a[href]') : null;
        if (target && target.href) {
            target.href = rewriteUrl(target.href);
        }
    }, true);

    var origFormSubmit = HTMLFormElement.prototype.submit;
    HTMLFormElement.prototype.submit = function() {
        if (this.action) {
            this.action = rewriteUrl(this.action);
        }
        return origFormSubmit.apply(this, arguments);
    };

    var origWindowOpen = window.open;
    if (origWindowOpen) {
        window.open = function(url) {
            if (typeof url === 'string') {
                arguments[0] = rewriteUrl(url);
            }
            return origWindowOpen.apply(this, arguments);
        };
    }

    rewriteDocument(document);
    if (document.documentElement && window.MutationObserver) {
        new MutationObserver(function(mutations) {
            mutations.forEach(function(mutation) {
                if (mutation.type === 'attributes' && mutation.target) {
                    rewriteElementAttr(mutation.target, mutation.attributeName);
                    return;
                }
                mutation.addedNodes.forEach(function(node) {
                    if (node.nodeType === 1) {
                        rewriteDocument(node);
                    }
                });
            });
        }).observe(document.documentElement, {
            subtree: true,
            childList: true,
            attributes: true,
            attributeFilter: ['href', 'src', 'action']
        });
    }
})();
</script>
`, localOrigin, upstreamOrigin)

	switch {
	case strings.Contains(html, "</head>"):
		return strings.Replace(html, "</head>", script+"</head>", 1)
	case strings.Contains(html, "</body>"):
		return strings.Replace(html, "</body>", script+"</body>", 1)
	default:
		return html + script
	}
}

func newCaptchaProxyTransport(dialer *dnsdialer.Dialer) *http.Transport {
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if dialer != nil {
		transport.DialContext = dialer.DialContext
	}

	return transport
}

func startCaptchaServer(srv *http.Server, logPrefix string) error {
	var listenErrs []string
	var listening bool

	for _, addr := range localCaptchaListenAddrs() {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			listenErrs = append(listenErrs, fmt.Sprintf("%s (%v)", addr, err))
			continue
		}
		listening = true
		go func(listener net.Listener) {
			if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Printf("%s: %s", logPrefix, err)
			}
		}(listener)
	}

	if listening {
		return nil
	}

	return fmt.Errorf("captcha listeners failed: %s", strings.Join(listenErrs, "; "))
}

func solveCaptchaViaHTTP(captchaImg string) (string, error) {
	keyCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:sans-serif;text-align:center;padding:20px}
img{max-width:100%%;margin:16px 0}
input{font-size:24px;padding:12px;width:80%%;box-sizing:border-box}
button{font-size:24px;padding:12px 32px;margin-top:12px;cursor:pointer}</style>
</head><body>
<h2>Введите капчу</h2>
<img src="%s" alt="captcha"/>
<form onsubmit="fetch('/solve?key='+encodeURIComponent(document.getElementById('k').value)).then(()=>{document.body.innerHTML='<h2>Готово</h2>';setTimeout(function(){window.close();}, 300);});return false;">
<br><input id="k" type="text" autofocus placeholder="Текст с картинки"/>
<br><button type="submit">Отправить</button>
</form></body></html>`, captchaImg)
	})
	mux.HandleFunc("/solve", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key != "" {
			select {
			case keyCh <- key:
			default:
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><h2>Готово</h2></body></html>`)
	})

	srv := &http.Server{
		Addr:    "localhost:" + captchaListenPort,
		Handler: mux,
	}

	if err := startCaptchaServer(srv, "captcha HTTP server error"); err != nil {
		return "", err
	}

	captchaURL := localCaptchaOrigin()
	fmt.Println("CAPTCHA_REQUIRED: " + captchaURL)
	openBrowser(captchaURL)

	key := <-keyCh

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	return key, nil
}

func solveCaptchaViaProxy(redirectURI string, dialer *dnsdialer.Dialer) (string, error) {
	keyCh := make(chan string, 1)

	targetURL, err := neturl.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("invalid redirect URI: %v", err)
	}
	transport := newCaptchaProxyTransport(dialer)

	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			rewriteProxyRequest(req, targetURL)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("captcha proxy error for %s: %v", r.URL.String(), err)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `<!DOCTYPE html><html><body style="font-family:sans-serif;padding:20px"><h2>Captcha proxy error</h2><p>%s</p><p>Try opening the link again after excluding your browser from the VPN on Android.</p></body></html>`, err)
		},
		ModifyResponse: func(res *http.Response) error {
			rewriteProxyCookies(res.Header)

			if res.StatusCode >= 300 && res.StatusCode < 400 && res.Header.Get("Location") != "" {
				loc := res.Header.Get("Location")
				if strings.HasPrefix(loc, "/") {
					res.Header.Set("Location", loc)
				} else if strings.HasPrefix(loc, targetOrigin(targetURL)) {
					res.Header.Set("Location", strings.Replace(loc, targetOrigin(targetURL), localCaptchaOrigin(), 1))
				}
			}

			contentType := res.Header.Get("Content-Type")
			shouldInspectBody := strings.Contains(contentType, "text/html") || strings.Contains(res.Request.URL.Path, "captchaNotRobot.check")
			if !shouldInspectBody {
				return nil
			}

			reader := res.Body
			if res.Header.Get("Content-Encoding") == "gzip" {
				gzReader, err := gzip.NewReader(res.Body)
				if err == nil {
					reader = gzReader
					defer gzReader.Close()
				}
			}

			bodyBytes, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			res.Body.Close()

			if strings.Contains(res.Request.URL.Path, "captchaNotRobot.check") {
				if token := extractSuccessToken(bodyBytes); token != "" {
					select {
					case keyCh <- token:
					default:
					}
				}
			}

			if strings.Contains(contentType, "text/html") {
				for _, headerName := range []string{
					"Content-Security-Policy",
					"Content-Security-Policy-Report-Only",
					"X-Content-Security-Policy",
					"X-WebKit-CSP",
					"Cross-Origin-Opener-Policy",
					"Cross-Origin-Embedder-Policy",
					"Cross-Origin-Resource-Policy",
					"X-Frame-Options",
				} {
					res.Header.Del(headerName)
				}

				bodyBytes = []byte(rewriteCaptchaHTML(string(bodyBytes), targetURL))
				res.Header.Del("Content-Encoding")
			}

			res.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			res.ContentLength = int64(len(bodyBytes))
			res.Header.Set("Content-Length", fmt.Sprint(len(bodyBytes)))

			return nil
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/local-captcha-result", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		token := r.FormValue("token")
		if token != "" {
			select {
			case keyCh <- token:
			default:
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/generic_proxy", func(w http.ResponseWriter, r *http.Request) {
		targetAuthUrl := r.URL.Query().Get("proxy_url")
		targetParsed, err := neturl.Parse(targetAuthUrl)
		if err != nil || targetParsed.Host == "" {
			http.Error(w, "Bad URL", 400)
			return
		}
		genericReverse := &httputil.ReverseProxy{
			Transport: transport,
			Director: func(req *http.Request) {
				req.URL.Path = targetParsed.Path
				req.URL.RawQuery = targetParsed.RawQuery
				rewriteProxyRequest(req, targetParsed)
			},
		}
		genericReverse.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && targetURL.Path != "" && targetURL.Path != "/" && r.URL.RawQuery == "" {
			http.Redirect(w, r, localCaptchaURLForTarget(targetURL), http.StatusTemporaryRedirect)
			return
		}
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    "localhost:" + captchaListenPort,
		Handler: mux,
	}

	if err := startCaptchaServer(srv, "proxy HTTP server error"); err != nil {
		return "", err
	}

	captchaURL := localCaptchaURLForTarget(targetURL)
	fmt.Println("CAPTCHA_REQUIRED: " + captchaURL)
	openBrowser(captchaURL)

	key := <-keyCh

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	return key, nil
}

func openBrowser(url string) {
	for _, cmd := range browserOpenCommands(runtime.GOOS, url) {
		if err := exec.Command(cmd.name, cmd.args...).Start(); err == nil {
			return
		}
	}
}

func browserOpenCommands(goos string, url string) []browserCommand {
	switch goos {
	case "windows":
		return []browserCommand{{name: "cmd", args: []string{"/c", "start", url}}}
	case "darwin":
		return []browserCommand{{name: "open", args: []string{url}}}
	case "linux":
		return []browserCommand{
			{name: "xdg-open", args: []string{url}},
			{name: "gio", args: []string{"open", url}},
		}
	case "android":
		return []browserCommand{
			{name: "termux-open-url", args: []string{url}},
			{name: "/system/bin/am", args: []string{"start", "-a", "android.intent.action.VIEW", "-d", url}},
			{name: "am", args: []string{"start", "-a", "android.intent.action.VIEW", "-d", url}},
			{name: "xdg-open", args: []string{url}},
		}
	case "ios":
		return []browserCommand{
			{name: "open", args: []string{url}},
			{name: "uiopen", args: []string{url}},
		}
	}

	return nil
}

func getVkCreds(link string, dialer *dnsdialer.Dialer) (string, string, string, error) {
	profile := getRandomProfile()
	log.Printf("Using User-Agent: %s\n", profile.UserAgent)

	doRequest := func(data string, url string) (resp map[string]interface{}, err error) {
		client := &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DialContext:         dialer.DialContext,
			},
		}
		defer client.CloseIdleConnections()

		req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(data)))
		if err != nil {
			return nil, err
		}

		req.Header.Add("User-Agent", profile.UserAgent)
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

		httpResp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() {
			if closeErr := httpResp.Body.Close(); closeErr != nil {
				log.Printf("close response body: %s", closeErr)
			}
		}()

		body, err := io.ReadAll(httpResp.Body)
		if err != nil {
			return nil, err
		}

		err = json.Unmarshal(body, &resp)
		if err != nil {
			return nil, err
		}

		return resp, nil
	}

	var resp map[string]interface{}
	defer func() {
		if r := recover(); r != nil {
			log.Panicf("get TURN creds error: %v\n\n", resp)
		}
	}()

	data := "client_id=6287487&token_type=messages&client_secret=QbYic1K3lEV5kTGiqlq2&version=1&app_id=6287487"
	url := "https://login.vk.ru/?act=get_anonym_token"

	resp, err := doRequest(data, url)
	if err != nil {
		return "", "", "", fmt.Errorf("request error:%s", err)
	}

	dataMap, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", "", "", fmt.Errorf("unexpected anon token response: %v", resp)
	}
	token1, ok := dataMap["access_token"].(string)
	if !ok {
		return "", "", "", fmt.Errorf("missing access_token in response: %v", resp)
	}

	data = fmt.Sprintf("vk_join_link=https://vk.com/call/join/%s&name=%s&access_token=%s", link, generateName(), token1)
	url = "https://api.vk.ru/method/calls.getAnonymousToken?v=5.274&client_id=6287487"

	var token2 string
	const maxCaptchaAttempts = 3
	for attempt := 0; attempt <= maxCaptchaAttempts; attempt++ {
		resp, err = doRequest(data, url)
		if err != nil {
			return "", "", "", fmt.Errorf("request error:%s", err)
		}

		// Check for captcha error
		if errObj, hasErr := resp["error"].(map[string]interface{}); hasErr {
			errCode, _ := errObj["error_code"].(float64)
			if errCode == 14 {
				if attempt == maxCaptchaAttempts {
					return "", "", "", fmt.Errorf("captcha failed after %d attempts", maxCaptchaAttempts)
				}

				captchaSid, _ := errObj["captcha_sid"].(string)
				if captchaSid == "" {
					// captcha_sid may be a number
					if sidNum, ok := errObj["captcha_sid"].(float64); ok {
						captchaSid = fmt.Sprintf("%.0f", sidNum)
					}
				}
				captchaImg, _ := errObj["captcha_img"].(string)
				redirectURI, _ := errObj["redirect_uri"].(string)

				log.Printf("Captcha required (attempt %d/%d), sid=%s", attempt+1, maxCaptchaAttempts, captchaSid)

				var solveErr error
				var successToken string
				var captchaKey string

				if redirectURI != "" {
					successToken, solveErr = solveCaptchaViaProxy(redirectURI, dialer)
					if solveErr != nil {
						return "", "", "", fmt.Errorf("proxy captcha solve error: %s", solveErr)
					}
					captchaTs, _ := errObj["captcha_ts"].(float64)
					captchaAttempt, _ := errObj["captcha_attempt"].(float64)
					if captchaAttempt == 0 {
						captchaAttempt = 1
					}

					data = fmt.Sprintf("vk_join_link=https://vk.com/call/join/%s&name=123&access_token=%s&captcha_key=&captcha_sid=%s&is_sound_captcha=0&success_token=%s&captcha_ts=%.3f&captcha_attempt=%d",
						link, token1, captchaSid, neturl.QueryEscape(successToken), captchaTs, int(captchaAttempt))
				} else {
					captchaKey, solveErr = solveCaptchaViaHTTP(captchaImg)
					if solveErr != nil {
						return "", "", "", fmt.Errorf("captcha solve error: %s", solveErr)
					}
					data = fmt.Sprintf("vk_join_link=https://vk.com/call/join/%s&name=123&access_token=%s&captcha_sid=%s&captcha_key=%s",
						link, token1, captchaSid, captchaKey)
				}
				continue
			}
			return "", "", "", fmt.Errorf("VK API error: %v", errObj)
		}

		respMap, ok := resp["response"].(map[string]interface{})
		if !ok {
			return "", "", "", fmt.Errorf("unexpected getAnonymousToken response: %v", resp)
		}
		token2, ok = respMap["token"].(string)
		if !ok {
			return "", "", "", fmt.Errorf("missing token in response: %v", resp)
		}
		break
	}

	data = fmt.Sprintf("%s%s%s", "session_data=%7B%22version%22%3A2%2C%22device_id%22%3A%22", uuid.New(), "%22%2C%22client_version%22%3A1.1%2C%22client_type%22%3A%22SDK_JS%22%7D&method=auth.anonymLogin&format=JSON&application_key=CGMMEJLGDIHBABABA")
	url = "https://calls.okcdn.ru/fb.do"

	resp, err = doRequest(data, url)
	if err != nil {
		return "", "", "", fmt.Errorf("request error:%s", err)
	}

	token3 := resp["session_key"].(string)

	data = fmt.Sprintf("joinLink=%s&isVideo=false&protocolVersion=5&anonymToken=%s&method=vchat.joinConversationByLink&format=JSON&application_key=CGMMEJLGDIHBABABA&session_key=%s", link, token2, token3)
	url = "https://calls.okcdn.ru/fb.do"

	resp, err = doRequest(data, url)
	if err != nil {
		return "", "", "", fmt.Errorf("request error:%s", err)
	}

	user := resp["turn_server"].(map[string]interface{})["username"].(string)
	pass := resp["turn_server"].(map[string]interface{})["credential"].(string)
	turn := resp["turn_server"].(map[string]interface{})["urls"].([]interface{})[0].(string)

	clean := strings.Split(turn, "?")[0]
	address := strings.TrimPrefix(strings.TrimPrefix(clean, "turn:"), "turns:")

	return user, pass, address, nil
}

func getYandexCreds(link string) (string, string, string, error) {
	const debug = false
	const telemostConfHost = "cloud-api.yandex.ru"
	telemostConfPath := fmt.Sprintf("%s%s%s", "/telemost_front/v2/telemost/conferences/https%3A%2F%2Ftelemost.yandex.ru%2Fj%2F", link, "/connection?next_gen_media_platform_allowed=false")
	profile := getRandomProfile()
	userAgent := profile.UserAgent

	type ConferenceResponse struct {
		URI                 string `json:"uri"`
		RoomID              string `json:"room_id"`
		PeerID              string `json:"peer_id"`
		ClientConfiguration struct {
			MediaServerURL string `json:"media_server_url"`
		} `json:"client_configuration"`
		Credentials string `json:"credentials"`
	}

	type PartMeta struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Description string `json:"description"`
		SendAudio   bool   `json:"sendAudio"`
		SendVideo   bool   `json:"sendVideo"`
	}

	type PartAttrs struct {
		Name        string `json:"name"`
		Role        string `json:"role"`
		Description string `json:"description"`
	}

	type SdkInfo struct {
		Implementation string `json:"implementation"`
		Version        string `json:"version"`
		UserAgent      string `json:"userAgent"`
		HwConcurrency  int    `json:"hwConcurrency"`
	}

	type Capabilities struct {
		OfferAnswerMode             []string `json:"offerAnswerMode"`
		InitialSubscriberOffer      []string `json:"initialSubscriberOffer"`
		SlotsMode                   []string `json:"slotsMode"`
		SimulcastMode               []string `json:"simulcastMode"`
		SelfVadStatus               []string `json:"selfVadStatus"`
		DataChannelSharing          []string `json:"dataChannelSharing"`
		VideoEncoderConfig          []string `json:"videoEncoderConfig"`
		DataChannelVideoCodec       []string `json:"dataChannelVideoCodec"`
		BandwidthLimitationReason   []string `json:"bandwidthLimitationReason"`
		SdkDefaultDeviceManagement  []string `json:"sdkDefaultDeviceManagement"`
		JoinOrderLayout             []string `json:"joinOrderLayout"`
		PinLayout                   []string `json:"pinLayout"`
		SendSelfViewVideoSlot       []string `json:"sendSelfViewVideoSlot"`
		ServerLayoutTransition      []string `json:"serverLayoutTransition"`
		SdkPublisherOptimizeBitrate []string `json:"sdkPublisherOptimizeBitrate"`
		SdkNetworkLostDetection     []string `json:"sdkNetworkLostDetection"`
		SdkNetworkPathMonitor       []string `json:"sdkNetworkPathMonitor"`
		PublisherVp9                []string `json:"publisherVp9"`
		SvcMode                     []string `json:"svcMode"`
		SubscriberOfferAsyncAck     []string `json:"subscriberOfferAsyncAck"`
		SvcModes                    []string `json:"svcModes"`
		ReportTelemetryModes        []string `json:"reportTelemetryModes"`
		KeepDefaultDevicesModes     []string `json:"keepDefaultDevicesModes"`
	}

	type HelloPayload struct {
		ParticipantMeta        PartMeta     `json:"participantMeta"`
		ParticipantAttributes  PartAttrs    `json:"participantAttributes"`
		SendAudio              bool         `json:"sendAudio"`
		SendVideo              bool         `json:"sendVideo"`
		SendSharing            bool         `json:"sendSharing"`
		ParticipantID          string       `json:"participantId"`
		RoomID                 string       `json:"roomId"`
		ServiceName            string       `json:"serviceName"`
		Credentials            string       `json:"credentials"`
		CapabilitiesOffer      Capabilities `json:"capabilitiesOffer"`
		SdkInfo                SdkInfo      `json:"sdkInfo"`
		SdkInitializationID    string       `json:"sdkInitializationId"`
		DisablePublisher       bool         `json:"disablePublisher"`
		DisableSubscriber      bool         `json:"disableSubscriber"`
		DisableSubscriberAudio bool         `json:"disableSubscriberAudio"`
	}

	type HelloRequest struct {
		UID   string       `json:"uid"`
		Hello HelloPayload `json:"hello"`
	}

	type FlexUrls []string

	type WSSResponse struct {
		UID         string `json:"uid"`
		ServerHello struct {
			RtcConfiguration struct {
				IceServers []struct {
					Urls       FlexUrls `json:"urls"`
					Username   string   `json:"username,omitempty"`
					Credential string   `json:"credential,omitempty"`
				} `json:"iceServers"`
			} `json:"rtcConfiguration"`
		} `json:"serverHello"`
	}

	type WSSAck struct {
		Uid string `json:"uid"`
		Ack struct {
			Status struct {
				Code string `json:"code"`
			} `json:"status"`
		} `json:"ack"`
	}

	type WSSData struct {
		ParticipantId string
		RoomId        string
		Credentials   string
		Wss           string
	}

	endpoint := "https://" + telemostConfHost + telemostConfPath
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: tr,
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://telemost.yandex.ru/")
	req.Header.Set("Origin", "https://telemost.yandex.ru")
	req.Header.Set("Client-Instance-Id", uuid.New().String())

	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("close response body: %s", closeErr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", "", "", fmt.Errorf("GetConference: status=%s body=%s", resp.Status, string(body))
	}

	var result ConferenceResponse
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", "", fmt.Errorf("decode conf: %v", err)
	}
	data := WSSData{
		ParticipantId: result.PeerID,
		RoomId:        result.RoomID,
		Credentials:   result.Credentials,
		Wss:           result.ClientConfiguration.MediaServerURL,
	}
	h := http.Header{}
	h.Set("Origin", "https://telemost.yandex.ru")
	h.Set("User-Agent", userAgent)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, data.Wss, h)
	if err != nil {
		return "", "", "", fmt.Errorf("ws dial: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			log.Printf("close websocket: %s", closeErr)
		}
	}()

	req1 := HelloRequest{
		UID: uuid.New().String(),
		Hello: HelloPayload{
			ParticipantMeta: PartMeta{
				Name:        generateName(),
				Role:        "SPEAKER",
				Description: "",
				SendAudio:   false,
				SendVideo:   false,
			},
			ParticipantAttributes: PartAttrs{
				Name:        generateName(),
				Role:        "SPEAKER",
				Description: "",
			},
			SendAudio:   false,
			SendVideo:   false,
			SendSharing: false,

			ParticipantID: data.ParticipantId,
			RoomID:        data.RoomId,
			ServiceName:   "telemost",
			Credentials:   data.Credentials,
			SdkInfo: SdkInfo{
				Implementation: "browser",
				Version:        "5.15.0",
				UserAgent:      userAgent,
				HwConcurrency:  4,
			},
			SdkInitializationID:    uuid.New().String(),
			DisablePublisher:       false,
			DisableSubscriber:      false,
			DisableSubscriberAudio: false,
			CapabilitiesOffer: Capabilities{
				OfferAnswerMode:             []string{"SEPARATE"},
				InitialSubscriberOffer:      []string{"ON_HELLO"},
				SlotsMode:                   []string{"FROM_CONTROLLER"},
				SimulcastMode:               []string{"DISABLED"},
				SelfVadStatus:               []string{"FROM_SERVER"},
				DataChannelSharing:          []string{"TO_RTP"},
				VideoEncoderConfig:          []string{"NO_CONFIG"},
				DataChannelVideoCodec:       []string{"VP8"},
				BandwidthLimitationReason:   []string{"BANDWIDTH_REASON_DISABLED"},
				SdkDefaultDeviceManagement:  []string{"SDK_DEFAULT_DEVICE_MANAGEMENT_DISABLED"},
				JoinOrderLayout:             []string{"JOIN_ORDER_LAYOUT_DISABLED"},
				PinLayout:                   []string{"PIN_LAYOUT_DISABLED"},
				SendSelfViewVideoSlot:       []string{"SEND_SELF_VIEW_VIDEO_SLOT_DISABLED"},
				ServerLayoutTransition:      []string{"SERVER_LAYOUT_TRANSITION_DISABLED"},
				SdkPublisherOptimizeBitrate: []string{"SDK_PUBLISHER_OPTIMIZE_BITRATE_DISABLED"},
				SdkNetworkLostDetection:     []string{"SDK_NETWORK_LOST_DETECTION_DISABLED"},
				SdkNetworkPathMonitor:       []string{"SDK_NETWORK_PATH_MONITOR_DISABLED"},
				PublisherVp9:                []string{"PUBLISH_VP9_DISABLED"},
				SvcMode:                     []string{"SVC_MODE_DISABLED"},
				SubscriberOfferAsyncAck:     []string{"SUBSCRIBER_OFFER_ASYNC_ACK_DISABLED"},
				SvcModes:                    []string{"FALSE"},
				ReportTelemetryModes:        []string{"TRUE"},
				KeepDefaultDevicesModes:     []string{"TRUE"},
			},
		},
	}

	if debug {
		b, _ := json.MarshalIndent(req1, "", "  ")
		log.Printf("Sending HELLO:\n%s", string(b))
	}

	if err := conn.WriteJSON(req1); err != nil {
		return "", "", "", fmt.Errorf("ws write: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return "", "", "", fmt.Errorf("ws set read deadline: %w", err)
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return "", "", "", fmt.Errorf("ws read: %w", err)
		}
		if debug {
			s := string(msg)
			if len(s) > 800 {
				s = s[:800] + "...(truncated)"
			}
			log.Printf("WSS recv: %s", s)
		}

		var ack WSSAck
		if err := json.Unmarshal(msg, &ack); err == nil && ack.Ack.Status.Code != "" {
			continue
		}

		var resp WSSResponse
		if err := json.Unmarshal(msg, &resp); err == nil {
			ice := resp.ServerHello.RtcConfiguration.IceServers
			for _, s := range ice {
				for _, u := range s.Urls {
					if !strings.HasPrefix(u, "turn:") && !strings.HasPrefix(u, "turns:") {
						continue
					}
					if strings.Contains(u, "transport=tcp") {
						continue
					}
					clean := strings.Split(u, "?")[0]
					address := strings.TrimPrefix(strings.TrimPrefix(clean, "turn:"), "turns:")

					return s.Username, s.Credential, address, nil
				}
			}
		}
	}
}

func dtlsFunc(ctx context.Context, conn net.PacketConn, peer *net.UDPAddr) (net.Conn, error) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, err
	}
	config := &dtls.Config{
		Certificates:          []tls.Certificate{certificate},
		InsecureSkipVerify:    true,
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.OnlySendCIDGenerator(),
	}
	ctx1, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dtlsConn, err := dtls.Client(conn, peer, config)
	if err != nil {
		return nil, err
	}

	if err := dtlsConn.HandshakeContext(ctx1); err != nil {
		return nil, err
	}
	return dtlsConn, nil
}

func oneDtlsConnection(ctx context.Context, peer *net.UDPAddr, listenConn net.PacketConn, connchan chan<- net.PacketConn, okchan chan<- struct{}, c chan<- error) {
	time.Sleep(time.Duration(rand.Intn(400)+100) * time.Millisecond)
	var err error = nil
	defer func() { c <- err }()
	dtlsctx, dtlscancel := context.WithCancel(ctx)
	defer dtlscancel()
	var conn1, conn2 net.PacketConn
	conn1, conn2 = connutil.AsyncPacketPipe()
	go func() {
		for {
			select {
			case <-dtlsctx.Done():
				return
			case connchan <- conn2:
			}
		}
	}()
	dtlsConn, err1 := dtlsFunc(dtlsctx, conn1, peer)
	if err1 != nil {
		err = fmt.Errorf("failed to connect DTLS: %s", err1)
		return
	}
	defer func() {
		if closeErr := dtlsConn.Close(); closeErr != nil {
			err = fmt.Errorf("failed to close DTLS connection: %s", closeErr)
			return
		}
		log.Printf("Closed DTLS connection\n")
	}()
	log.Printf("Established DTLS connection!\n")
	go func() {
		for {
			select {
			case <-dtlsctx.Done():
				return
			case okchan <- struct{}{}:
			}
		}
	}()

	wg := sync.WaitGroup{}
	wg.Add(2)
	context.AfterFunc(dtlsctx, func() {
		if err := listenConn.SetDeadline(time.Now()); err != nil {
			log.Printf("Failed to set listener deadline: %s", err)
		}
		if err := dtlsConn.SetDeadline(time.Now()); err != nil {
			log.Printf("Failed to set DTLS deadline: %s", err)
		}
	})
	var addr atomic.Value
	// Start read-loop on listenConn
	go func() {
		defer wg.Done()
		defer dtlscancel()
		buf := make([]byte, 1600)
		for {
			select {
			case <-dtlsctx.Done():
				return
			default:
			}
			n, addr1, err1 := listenConn.ReadFrom(buf)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}

			addr.Store(addr1) // store peer

			_, err1 = dtlsConn.Write(buf[:n])
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
		}
	}()

	// Start read-loop on dtlsConn
	go func() {
		defer wg.Done()
		defer dtlscancel()
		buf := make([]byte, 1600)
		for {
			select {
			case <-dtlsctx.Done():
				return
			default:
			}
			n, err1 := dtlsConn.Read(buf)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
			addr1, ok := addr.Load().(net.Addr)
			if !ok {
				log.Printf("Failed: no listener ip")
				return
			}

			_, err1 = listenConn.WriteTo(buf[:n], addr1)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
		}
	}()

	wg.Wait()
	if err := listenConn.SetDeadline(time.Time{}); err != nil {
		log.Printf("Failed to clear listener deadline: %s", err)
	}
	if err := dtlsConn.SetDeadline(time.Time{}); err != nil {
		log.Printf("Failed to clear DTLS deadline: %s", err)
	}
}

type connectedUDPConn struct {
	*net.UDPConn
}

func (c *connectedUDPConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.Write(p)
}

type turnParams struct {
	host     string
	port     string
	link     string
	udp      bool
	getCreds getCredsFunc
}

func oneTurnConnection(ctx context.Context, turnParams *turnParams, peer *net.UDPAddr, conn2 net.PacketConn, c chan<- error) {
	time.Sleep(time.Duration(rand.Intn(400)+100) * time.Millisecond)
	var err error = nil
	defer func() { c <- err }()
	user, pass, url, err1 := turnParams.getCreds(turnParams.link)
	if err1 != nil {
		err = fmt.Errorf("failed to get TURN credentials: %s", err1)
		return
	}
	urlhost, urlport, err1 := net.SplitHostPort(url)
	if err1 != nil {
		err = fmt.Errorf("failed to parse TURN server address: %s", err1)
		return
	}
	if turnParams.host != "" {
		urlhost = turnParams.host
	}
	if turnParams.port != "" {
		urlport = turnParams.port
	}
	var turnServerAddr string
	turnServerAddr = net.JoinHostPort(urlhost, urlport)
	turnServerUdpAddr, err1 := net.ResolveUDPAddr("udp", turnServerAddr)
	if err1 != nil {
		err = fmt.Errorf("failed to resolve TURN server address: %s", err1)
		return
	}
	turnServerAddr = turnServerUdpAddr.String()
	fmt.Println(turnServerUdpAddr.IP)
	// Dial TURN Server
	var cfg *turn.ClientConfig
	var turnConn net.PacketConn
	var d net.Dialer
	ctx1, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if turnParams.udp {
		conn, err2 := net.DialUDP("udp", nil, turnServerUdpAddr) // nolint: noctx
		if err2 != nil {
			err = fmt.Errorf("failed to connect to TURN server: %s", err2)
			return
		}
		defer func() {
			if err1 = conn.Close(); err1 != nil {
				err = fmt.Errorf("failed to close TURN server connection: %s", err1)
				return
			}
		}()
		turnConn = &connectedUDPConn{conn}
	} else {
		conn, err2 := d.DialContext(ctx1, "tcp", turnServerAddr) // nolint: noctx
		if err2 != nil {
			err = fmt.Errorf("failed to connect to TURN server: %s", err2)
			return
		}
		defer func() {
			if err1 = conn.Close(); err1 != nil {
				err = fmt.Errorf("failed to close TURN server connection: %s", err1)
				return
			}
		}()
		turnConn = turn.NewSTUNConn(conn)
	}
	var addrFamily turn.RequestedAddressFamily
	if peer.IP.To4() != nil {
		addrFamily = turn.RequestedAddressFamilyIPv4
	} else {
		addrFamily = turn.RequestedAddressFamilyIPv6
	}
	// Start a new TURN Client and wrap our net.Conn in a STUNConn
	// This allows us to simulate datagram based communication over a net.Conn
	cfg = &turn.ClientConfig{
		STUNServerAddr:         turnServerAddr,
		TURNServerAddr:         turnServerAddr,
		Conn:                   turnConn,
		Net:                    newDirectNet(),
		Username:               user,
		Password:               pass,
		RequestedAddressFamily: addrFamily,
		LoggerFactory:          logging.NewDefaultLoggerFactory(),
	}

	client, err1 := turn.NewClient(cfg)
	if err1 != nil {
		err = fmt.Errorf("failed to create TURN client: %s", err1)
		return
	}
	defer client.Close()

	// Start listening on the conn provided.
	err1 = client.Listen()
	if err1 != nil {
		err = fmt.Errorf("failed to listen: %s", err1)
		return
	}

	// Allocate a relay socket on the TURN server. On success, it
	// will return a net.PacketConn which represents the remote
	// socket.
	relayConn, err1 := client.Allocate()
	if err1 != nil {
		err = fmt.Errorf("failed to allocate: %s", err1)
		return
	}
	defer func() {
		if err1 := relayConn.Close(); err1 != nil {
			err = fmt.Errorf("failed to close TURN allocated connection: %s", err1)
		}
	}()

	// The relayConn's local address is actually the transport
	// address assigned on the TURN server.
	log.Printf("relayed-address=%s", relayConn.LocalAddr().String())

	wg := sync.WaitGroup{}
	wg.Add(2)
	turnctx, turncancel := context.WithCancel(context.Background())
	context.AfterFunc(turnctx, func() {
		if err := relayConn.SetDeadline(time.Now()); err != nil {
			log.Printf("Failed to set relay deadline: %s", err)
		}
		if err := conn2.SetDeadline(time.Now()); err != nil {
			log.Printf("Failed to set upstream deadline: %s", err)
		}
	})
	var addr atomic.Value
	// Start read-loop on conn2 (output of DTLS)
	go func() {
		defer wg.Done()
		defer turncancel()
		buf := make([]byte, 1600)
		for {
			select {
			case <-turnctx.Done():
				return
			default:
			}
			n, addr1, err1 := conn2.ReadFrom(buf)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}

			addr.Store(addr1) // store peer

			_, err1 = relayConn.WriteTo(buf[:n], peer)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
		}
	}()

	// Start read-loop on relayConn
	go func() {
		defer wg.Done()
		defer turncancel()
		buf := make([]byte, 1600)
		for {
			select {
			case <-turnctx.Done():
				return
			default:
			}
			n, _, err1 := relayConn.ReadFrom(buf)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
			addr1, ok := addr.Load().(net.Addr)
			if !ok {
				log.Printf("Failed: no listener ip")
				return
			}

			_, err1 = conn2.WriteTo(buf[:n], addr1)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
		}
	}()

	wg.Wait()
	if err := relayConn.SetDeadline(time.Time{}); err != nil {
		log.Printf("Failed to clear relay deadline: %s", err)
	}
	if err := conn2.SetDeadline(time.Time{}); err != nil {
		log.Printf("Failed to clear upstream deadline: %s", err)
	}
}

func oneDtlsConnectionLoop(ctx context.Context, peer *net.UDPAddr, listenConnChan <-chan net.PacketConn, connchan chan<- net.PacketConn, okchan chan<- struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case listenConn := <-listenConnChan:
			c := make(chan error)
			go oneDtlsConnection(ctx, peer, listenConn, connchan, okchan, c)
			if err := <-c; err != nil {
				log.Printf("%s", err)
			}
		}
	}
}

func oneTurnConnectionLoop(ctx context.Context, turnParams *turnParams, peer *net.UDPAddr, connchan <-chan net.PacketConn, t <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case conn2 := <-connchan:
			select {
			case <-t:
				c := make(chan error)
				go oneTurnConnection(ctx, turnParams, peer, conn2, c)
				if err := <-c; err != nil {
					log.Printf("%s", err)
				}
			default:
			}
		}
	}
}

func cachedCreds(f getCredsFunc) getCredsFunc {
	var mu sync.Mutex
	var cUser, cPass, cAddr string
	var cTime time.Time
	return func(link string) (string, string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		if !cTime.IsZero() && time.Since(cTime) < 10*time.Minute {
			return cUser, cPass, cAddr, nil
		}
		u, p, a, err := f(link)
		if err == nil {
			cUser, cPass, cAddr = u, p, a
			cTime = time.Now()
		}
		return u, p, a, err
	}
}

func main() { //nolint:cyclop
	rand.Seed(time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signalChan
		log.Printf("Terminating...\n")
		cancel()
		select {
		case <-signalChan:
		case <-time.After(5 * time.Second):
		}
		log.Fatalf("Exit...\n")
	}()

	host := flag.String("turn", "", "override TURN server ip")
	port := flag.String("port", "", "override TURN port")
	listen := flag.String("listen", "127.0.0.1:9000", "listen on ip:port")
	vklink := flag.String("vk-link", "", "VK calls invite link \"https://vk.com/call/join/...\"")
	yalink := flag.String("yandex-link", "", "Yandex telemost invite link \"https://telemost.yandex.ru/j/...\"")
	peerAddr := flag.String("peer", "", "peer server address (host:port)")
	n := flag.Int("n", 0, "connections to TURN (default 10 for VK, 1 for Yandex)")
	udp := flag.Bool("udp", false, "connect to TURN with UDP")
	direct := flag.Bool("no-dtls", false, "connect without obfuscation. DO NOT USE")
	tcpMode := flag.Bool("tcp", false, "TCP mode: forward TCP connections (for VLESS) instead of UDP packets")
	flag.Parse()
	if *peerAddr == "" {
		log.Panicf("Need peer address!")
	}
	peer, err := net.ResolveUDPAddr("udp", *peerAddr)
	if err != nil {
		panic(err)
	}
	if (*vklink == "") == (*yalink == "") {
		log.Panicf("Need either vk-link or yandex-link!")
	}

	var link string
	var getCreds getCredsFunc
	if *vklink != "" {
		parts := strings.Split(*vklink, "join/")
		link = parts[len(parts)-1]

		dialer := dnsdialer.New(
			dnsdialer.WithResolvers("77.88.8.8:53", "77.88.8.1:53", "8.8.8.8:53", "8.8.4.4:53", "1.1.1.1:53"),
			dnsdialer.WithStrategy(dnsdialer.Fallback{}),
			dnsdialer.WithCache(100, 10*time.Hour, 10*time.Hour),
		)

		getCreds = func(s string) (string, string, string, error) {
			return getVkCreds(s, dialer)
		}
		if *n <= 0 {
			*n = 10
		}
	} else {
		parts := strings.Split(*yalink, "j/")
		link = parts[len(parts)-1]
		getCreds = getYandexCreds
		if *n <= 0 {
			*n = 1
		}
	}
	if idx := strings.IndexAny(link, "/?#"); idx != -1 {
		link = link[:idx]
	}
	params := &turnParams{
		*host,
		*port,
		link,
		*udp,
		cachedCreds(getCreds),
	}

	if *tcpMode {
		runTCPMode(ctx, params, peer, *listen, *n)
		return
	}

	listenConnChan := make(chan net.PacketConn)
	listenConn, err := net.ListenPacket("udp", *listen) // nolint: noctx
	if err != nil {
		log.Panicf("Failed to listen: %s", err)
	}
	context.AfterFunc(ctx, func() {
		if closeErr := listenConn.Close(); closeErr != nil {
			log.Panicf("Failed to close local connection: %s", closeErr)
		}
	})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case listenConnChan <- listenConn:
			}
		}
	}()

	wg1 := sync.WaitGroup{}
	t := time.Tick(200 * time.Millisecond)
	if *direct {
		for i := 0; i < *n; i++ {
			wg1.Go(func() {
				oneTurnConnectionLoop(ctx, params, peer, listenConnChan, t)
			})
		}
	} else {
		okchan := make(chan struct{})
		connchan := make(chan net.PacketConn)

		wg1.Go(func() {
			oneDtlsConnectionLoop(ctx, peer, listenConnChan, connchan, okchan)
		})

		wg1.Go(func() {
			oneTurnConnectionLoop(ctx, params, peer, connchan, t)
		})

		select {
		case <-okchan:
		case <-ctx.Done():
		}
		for i := 0; i < *n-1; i++ {
			connchan := make(chan net.PacketConn)
			wg1.Go(func() {
				oneDtlsConnectionLoop(ctx, peer, listenConnChan, connchan, nil)
			})
			wg1.Go(func() {
				oneTurnConnectionLoop(ctx, params, peer, connchan, t)
			})
		}
	}

	wg1.Wait()
}

// sessionPool manages a pool of smux sessions for round-robin TCP distribution.
type sessionPool struct {
	mu       sync.RWMutex
	sessions []*smux.Session
	counter  atomic.Uint64
}

func (p *sessionPool) add(s *smux.Session) {
	p.mu.Lock()
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
}

func (p *sessionPool) remove(s *smux.Session) {
	p.mu.Lock()
	for i, sess := range p.sessions {
		if sess == s {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
}

func (p *sessionPool) pick() *smux.Session {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.sessions)
	if n == 0 {
		return nil
	}
	idx := p.counter.Add(1) % uint64(n)
	return p.sessions[idx]
}

func (p *sessionPool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.sessions)
}

// runTCPMode implements TCP forwarding with round-robin across N TURN sessions.
func runTCPMode(ctx context.Context, tp *turnParams, peer *net.UDPAddr, listenAddr string, numSessions int) {
	pool := &sessionPool{}

	// Start N session maintainers with staggered startup
	var wgMaint sync.WaitGroup
	for i := 0; i < numSessions; i++ {
		wgMaint.Add(1)
		go func(id int) {
			defer wgMaint.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(id) * 300 * time.Millisecond):
			}
			maintainTCPSession(ctx, tp, peer, id, pool)
		}(i)
	}

	// Wait for at least one session
	log.Printf("TCP mode: waiting for sessions to connect (total: %d)...", numSessions)
	for {
		select {
		case <-ctx.Done():
			wgMaint.Wait()
			return
		case <-time.After(100 * time.Millisecond):
		}
		if pool.count() > 0 {
			break
		}
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Panicf("TCP listen: %s", err)
	}
	context.AfterFunc(ctx, func() { listener.Close() })
	log.Printf("TCP mode: listening on %s (round-robin across %d sessions)", listenAddr, numSessions)

	var wgConn sync.WaitGroup
	for {
		tcpConn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				wgConn.Wait()
				wgMaint.Wait()
				return
			default:
			}
			log.Printf("TCP accept error: %s", err)
			continue
		}

		sess := pool.pick()
		if sess == nil || sess.IsClosed() {
			log.Printf("No active sessions, rejecting connection")
			tcpConn.Close()
			continue
		}

		wgConn.Add(1)
		go func(tc net.Conn, s *smux.Session) {
			defer wgConn.Done()
			defer tc.Close()
			stream, err := s.OpenStream()
			if err != nil {
				log.Printf("smux open stream error: %s", err)
				return
			}
			defer stream.Close()
			pipe(ctx, tc, stream)
		}(tcpConn, sess)
	}
}

// maintainTCPSession keeps one TURN+DTLS+KCP+smux session alive, reconnecting on failure.
func maintainTCPSession(ctx context.Context, tp *turnParams, peer *net.UDPAddr, id int, pool *sessionPool) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		smuxSess, cleanup, err := createSmuxSession(ctx, tp, peer)
		if err != nil {
			log.Printf("[session %d] setup error: %s, retrying...", id, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		pool.add(smuxSess)
		log.Printf("[session %d] connected (active: %d)", id, pool.count())

		for !smuxSess.IsClosed() {
			select {
			case <-ctx.Done():
				pool.remove(smuxSess)
				cleanup()
				return
			case <-time.After(1 * time.Second):
			}
		}

		pool.remove(smuxSess)
		cleanup()
		log.Printf("[session %d] disconnected (active: %d), reconnecting...", id, pool.count())

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// createSmuxSession establishes a full TURN+DTLS+KCP+smux pipeline and returns
// the smux session along with a cleanup function to tear down all layers.
func createSmuxSession(ctx context.Context, tp *turnParams, peer *net.UDPAddr) (*smux.Session, func(), error) {
	var cleanupFns []func()
	cleanup := func() {
		for i := len(cleanupFns) - 1; i >= 0; i-- {
			cleanupFns[i]()
		}
	}

	// 1. Get TURN credentials
	user, pass, rawURL, err := tp.getCreds(tp.link)
	if err != nil {
		return nil, nil, fmt.Errorf("get TURN creds: %w", err)
	}
	urlhost, urlport, err := net.SplitHostPort(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse TURN addr: %w", err)
	}
	if tp.host != "" {
		urlhost = tp.host
	}
	if tp.port != "" {
		urlport = tp.port
	}
	turnServerAddr := net.JoinHostPort(urlhost, urlport)
	turnServerUdpAddr, err := net.ResolveUDPAddr("udp", turnServerAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve TURN addr: %w", err)
	}
	turnServerAddr = turnServerUdpAddr.String()

	// 2. Connect to TURN server
	var turnConn net.PacketConn
	ctx1, cancel1 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel1()
	if tp.udp {
		conn, err := net.DialUDP("udp", nil, turnServerUdpAddr)
		if err != nil {
			return nil, nil, fmt.Errorf("dial TURN (udp): %w", err)
		}
		cleanupFns = append(cleanupFns, func() { conn.Close() })
		turnConn = &connectedUDPConn{conn}
	} else {
		var d net.Dialer
		conn, err := d.DialContext(ctx1, "tcp", turnServerAddr)
		if err != nil {
			return nil, nil, fmt.Errorf("dial TURN (tcp): %w", err)
		}
		cleanupFns = append(cleanupFns, func() { conn.Close() })
		turnConn = turn.NewSTUNConn(conn)
	}

	// 3. Create TURN client and allocate relay
	var addrFamily turn.RequestedAddressFamily
	if peer.IP.To4() != nil {
		addrFamily = turn.RequestedAddressFamilyIPv4
	} else {
		addrFamily = turn.RequestedAddressFamilyIPv6
	}
	cfg := &turn.ClientConfig{
		STUNServerAddr:         turnServerAddr,
		TURNServerAddr:         turnServerAddr,
		Conn:                   turnConn,
		Net:                    newDirectNet(),
		Username:               user,
		Password:               pass,
		RequestedAddressFamily: addrFamily,
		LoggerFactory:          logging.NewDefaultLoggerFactory(),
	}
	turnClient, err := turn.NewClient(cfg)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create TURN client: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { turnClient.Close() })
	if err = turnClient.Listen(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("TURN listen: %w", err)
	}
	relayConn, err := turnClient.Allocate()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("TURN allocate: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { relayConn.Close() })
	log.Printf("relayed-address=%s", relayConn.LocalAddr().String())

	// 4. Establish DTLS over TURN relay
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("generate cert: %w", err)
	}
	dtlsPC := &relayPacketConn{relay: relayConn, peer: peer}
	dtlsConfig := &dtls.Config{
		Certificates:          []tls.Certificate{certificate},
		InsecureSkipVerify:    true,
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.OnlySendCIDGenerator(),
	}
	dtlsConn, err := dtls.Client(dtlsPC, peer, dtlsConfig)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("DTLS client create: %w", err)
	}
	ctx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()
	if err = dtlsConn.HandshakeContext(ctx2); err != nil {
		dtlsConn.Close()
		cleanup()
		return nil, nil, fmt.Errorf("DTLS handshake: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { dtlsConn.Close() })
	log.Printf("DTLS connection established")

	// 5. Create KCP session over DTLS
	kcpSess, err := tcputil.NewKCPOverDTLS(dtlsConn, false)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("KCP session: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { kcpSess.Close() })
	log.Printf("KCP session established")

	// 6. Create smux client session over KCP
	smuxSess, err := smux.Client(kcpSess, tcputil.DefaultSmuxConfig())
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("smux client: %w", err)
	}
	cleanupFns = append(cleanupFns, func() { smuxSess.Close() })
	log.Printf("smux session established")

	return smuxSess, cleanup, nil
}

// relayPacketConn wraps a TURN relay PacketConn to direct all writes to the peer.
type relayPacketConn struct {
	relay net.PacketConn
	peer  net.Addr
}

func (r *relayPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	return r.relay.ReadFrom(b)
}

func (r *relayPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return r.relay.WriteTo(b, r.peer)
}

func (r *relayPacketConn) Close() error                       { return r.relay.Close() }
func (r *relayPacketConn) LocalAddr() net.Addr                { return r.relay.LocalAddr() }
func (r *relayPacketConn) SetDeadline(t time.Time) error      { return r.relay.SetDeadline(t) }
func (r *relayPacketConn) SetReadDeadline(t time.Time) error  { return r.relay.SetReadDeadline(t) }
func (r *relayPacketConn) SetWriteDeadline(t time.Time) error { return r.relay.SetWriteDeadline(t) }

// pipe copies data bidirectionally between two connections.
func pipe(ctx context.Context, c1, c2 net.Conn) {
	ctx2, cancel := context.WithCancel(ctx)
	context.AfterFunc(ctx2, func() {
		c1.SetDeadline(time.Now())
		c2.SetDeadline(time.Now())
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		io.Copy(c1, c2)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		io.Copy(c2, c1)
	}()
	wg.Wait()
	c1.SetDeadline(time.Time{})
	c2.SetDeadline(time.Time{})
}
