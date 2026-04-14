package jazz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cacggghp/vk-turn-proxy/internal/namegen"
	"github.com/google/uuid"
)

const defaultAPIBaseURL = "https://bk.salutejazz.ru"

var (
	apiBaseURL    = defaultAPIBaseURL
	apiHTTPClient = &http.Client{Timeout: 15 * time.Second}
)

var (
	errCreateRoomFailed = errors.New("create room failed")
	errPreconnectFailed = errors.New("preconnect failed")
)

type RoomInfo struct {
	RoomID       string `json:"roomId"`
	Password     string `json:"password"`
	ConnectorURL string `json:"connectorUrl"`
}

func CreateRoom(ctx context.Context) (*RoomInfo, error) {
	headers := jazzHeaders()

	createResp, err := createMeeting(ctx, headers)
	if err != nil {
		return nil, fmt.Errorf("create meeting: %w", err)
	}

	connectorURL, err := preconnect(ctx, createResp.RoomID, createResp.Password, headers)
	if err != nil {
		return nil, fmt.Errorf("preconnect: %w", err)
	}

	return &RoomInfo{
		RoomID:       createResp.RoomID,
		Password:     createResp.Password,
		ConnectorURL: connectorURL,
	}, nil
}

func JoinRoom(ctx context.Context, roomInput string) (*RoomInfo, error) {
	roomID, password := parseRoomInput(roomInput)
	if roomID == "" {
		return nil, fmt.Errorf("jazz room is required")
	}

	connectorURL, err := preconnect(ctx, roomID, password, jazzHeaders())
	if err != nil {
		return nil, err
	}

	return &RoomInfo{
		RoomID:       roomID,
		Password:     password,
		ConnectorURL: connectorURL,
	}, nil
}

func parseRoomInput(roomInput string) (string, string) {
	roomInput = strings.TrimSpace(roomInput)
	roomInput = strings.TrimPrefix(roomInput, "https://salutejazz.ru/")
	roomInput = strings.TrimPrefix(roomInput, "http://salutejazz.ru/")
	roomInput = strings.TrimPrefix(roomInput, "https://jazz.sber.ru/")
	roomInput = strings.TrimPrefix(roomInput, "http://jazz.sber.ru/")
	if idx := strings.IndexAny(roomInput, "/?#"); idx != -1 {
		roomInput = roomInput[:idx]
	}

	roomID, password, _ := strings.Cut(roomInput, ":")
	return strings.TrimSpace(roomID), strings.TrimSpace(password)
}

func jazzHeaders() map[string]string {
	return map[string]string{
		"X-Jazz-ClientId":   uuid.New().String(),
		"X-Jazz-AuthType":   "ANONYMOUS",
		"X-Client-AuthType": "ANONYMOUS",
		"Content-Type":      "application/json",
	}
}

type createResponse struct {
	RoomID   string `json:"roomId"`
	Password string `json:"password"`
}

func createMeeting(ctx context.Context, headers map[string]string) (*createResponse, error) {
	createPayload := map[string]any{
		"title":                             namegen.Generate() + " ДР",
		"guestEnabled":                      true,
		"lobbyEnabled":                      false,
		"serverVideoRecordAutoStartEnabled": false,
		"sipEnabled":                        false,
		"moderatorEmails":                   []string{},
		"summarizationEnabled":              false,
		"room3dEnabled":                     false,
		"room3dScene":                       "XRLobby",
	}

	body, err := json.Marshal(createPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal create payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/room/create-meeting", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	setHeaders(req, headers)

	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do create request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError(errCreateRoomFailed, resp)
	}

	var res createResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode create response: %w", err)
	}

	return &res, nil
}

func preconnect(ctx context.Context, roomID, password string, headers map[string]string) (string, error) {
	preconnectPayload := map[string]any{
		"password": password,
		"jazzNextMigration": map[string]any{
			"b2bBaseRoomSupport":               true,
			"demoRoomBaseSupport":              true,
			"demoRoomVersionSupport":           2,
			"mediaWithoutAutoSubscribeSupport": true,
			"webinarSpeakerSupport":            true,
			"webinarViewerSupport":             true,
			"sdkRoomSupport":                   true,
			"sberclassRoomSupport":             true,
		},
	}

	body, err := json.Marshal(preconnectPayload)
	if err != nil {
		return "", fmt.Errorf("marshal preconnect payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/room/%s/preconnect", apiBaseURL, roomID), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create preconnect request: %w", err)
	}
	setHeaders(req, headers)

	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do preconnect request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", statusError(errPreconnectFailed, resp)
	}

	var preconnectResp struct {
		ConnectorURL string `json:"connectorUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&preconnectResp); err != nil {
		return "", fmt.Errorf("decode preconnect response: %w", err)
	}
	if preconnectResp.ConnectorURL == "" {
		return "", fmt.Errorf("preconnect response missing connector URL")
	}

	return preconnectResp.ConnectorURL, nil
}

func setHeaders(req *http.Request, headers map[string]string) {
	for key, value := range headers {
		req.Header.Set(key, value)
	}
}

func statusError(base error, resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: status %s and response body read failed: %v", base, resp.Status, err)
	}
	return fmt.Errorf("%w: status %s: %s", base, resp.Status, strings.TrimSpace(string(body)))
}
