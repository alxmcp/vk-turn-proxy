package telemost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const defaultAPIBaseURL = "https://cloud-api.yandex.ru/telemost_front/v2/telemost"

var (
	apiBaseURL    = defaultAPIBaseURL
	apiHTTPClient = &http.Client{Timeout: 15 * time.Second}
)

type ConnectionInfo struct {
	RoomID       string `json:"room_id"`
	PeerID       string `json:"peer_id"`
	Credentials  string `json:"credentials"`
	ClientConfig struct {
		MediaServerURL string `json:"media_server_url"`
	} `json:"client_configuration"`
}

type RoomTarget struct {
	RoomID        string
	PreferredHost string
}

func ParseRoomTarget(input string) (RoomTarget, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return RoomTarget{}, fmt.Errorf("telemost invite link is required")
	}

	target := RoomTarget{}
	if strings.HasPrefix(input, "https://") || strings.HasPrefix(input, "http://") {
		parsed, err := url.Parse(input)
		if err != nil {
			return RoomTarget{}, fmt.Errorf("invalid telemost invite link: %w", err)
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 2 && parts[0] == "j" && parts[1] != "" {
			target.RoomID = parts[1]
			target.PreferredHost = strings.ToLower(parsed.Hostname())
		}
	}

	if target.RoomID == "" {
		input = strings.TrimPrefix(input, "https://telemost.yandex.ru/j/")
		input = strings.TrimPrefix(input, "http://telemost.yandex.ru/j/")
		input = strings.TrimPrefix(input, "https://telemost.yandex.com/j/")
		input = strings.TrimPrefix(input, "http://telemost.yandex.com/j/")
		input = strings.TrimPrefix(input, "j/")
		if idx := strings.IndexAny(input, "/?#"); idx != -1 {
			input = input[:idx]
		}
		target.RoomID = strings.TrimSpace(input)
	}

	if target.RoomID == "" {
		return RoomTarget{}, fmt.Errorf("invalid telemost invite link")
	}

	return target, nil
}

func (t RoomTarget) CandidateRoomURLs() []string {
	hosts := make([]string, 0, 3)
	addHost := func(host string) {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return
		}
		for _, existing := range hosts {
			if existing == host {
				return
			}
		}
		hosts = append(hosts, host)
	}

	addHost(t.PreferredHost)
	addHost("telemost.yandex.ru")
	addHost("telemost.yandex.com")

	roomURLs := make([]string, 0, len(hosts))
	for _, host := range hosts {
		roomURLs = append(roomURLs, fmt.Sprintf("https://%s/j/%s", host, t.RoomID))
	}

	return roomURLs
}

func WebOriginFromRoomURL(roomURL string) string {
	parsed, err := url.Parse(roomURL)
	if err != nil || parsed.Host == "" {
		return "https://telemost.yandex.ru"
	}

	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}

	return scheme + "://" + parsed.Host
}

func getConnectionInfoForRoomURL(ctx context.Context, roomURL, displayName string) (*ConnectionInfo, error) {
	u := fmt.Sprintf("%s/conferences/%s/connection", apiBaseURL, url.QueryEscape(roomURL))
	origin := WebOriginFromRoomURL(roomURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("next_gen_media_platform_allowed", "true")
	q.Add("display_name", displayName)
	q.Add("waiting_room_supported", "true")
	req.URL.RawQuery = q.Encode()

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Client-Instance-Id", uuid.New().String())
	req.Header.Set("X-Telemost-Client-Version", "187.1.0")
	req.Header.Set("Idempotency-Key", uuid.New().String())
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")

	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				log.Println(closeErr)
			}
		}
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Println(err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("telemost connection API returned %s and response body read failed: %w", resp.Status, readErr)
		}
		return nil, fmt.Errorf("telemost connection API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var info ConnectionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}

	return &info, nil
}

func GetConnectionInfo(ctx context.Context, roomInput, displayName string) (*ConnectionInfo, string, error) {
	target, err := ParseRoomTarget(roomInput)
	if err != nil {
		return nil, "", err
	}

	var errs []string
	for _, roomURL := range target.CandidateRoomURLs() {
		info, err := getConnectionInfoForRoomURL(ctx, roomURL, displayName)
		if err == nil {
			return info, roomURL, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, "", err
		}
		errs = append(errs, fmt.Sprintf("%s: %v", roomURL, err))
	}

	return nil, "", fmt.Errorf("telemost connection failed for room %s: %s", target.RoomID, strings.Join(errs, "; "))
}
