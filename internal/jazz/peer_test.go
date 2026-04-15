package jazz

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestHandleRTCConfigUsesRelayOnlyICEPolicy(t *testing.T) {
	pcSub, err := webrtc.NewPeerConnection(jazzWebRTCConfiguration(nil))
	if err != nil {
		t.Fatalf("NewPeerConnection(sub) error = %v", err)
	}
	defer func() { _ = pcSub.Close() }()

	pcPub, err := webrtc.NewPeerConnection(jazzWebRTCConfiguration(nil))
	if err != nil {
		t.Fatalf("NewPeerConnection(pub) error = %v", err)
	}
	defer func() { _ = pcPub.Close() }()

	session := &peerSession{
		pcSub: pcSub,
		pcPub: pcPub,
	}
	peer := &Peer{}

	peer.handleRTCConfig(session, map[string]any{
		"configuration": map[string]any{
			"iceServers": []any{
				map[string]any{
					"urls":       []any{"turn:a-t.salutejazz.ru:3478?transport=udp"},
					"username":   "user",
					"credential": "pass",
				},
			},
		},
	})

	if got := pcSub.GetConfiguration().ICETransportPolicy; got != webrtc.ICETransportPolicyRelay {
		t.Fatalf("subscriber ICETransportPolicy = %s, want relay", got)
	}
	if got := pcPub.GetConfiguration().ICETransportPolicy; got != webrtc.ICETransportPolicyRelay {
		t.Fatalf("publisher ICETransportPolicy = %s, want relay", got)
	}
}

func TestLocalICECandidateMessageUsesJazzRTCICEPayload(t *testing.T) {
	sdpMid := "0"
	sdpMLineIndex := uint16(0)
	candidate := webrtc.ICECandidateInit{
		Candidate:     "candidate:1 1 udp 2130706431 172.17.0.10 50000 typ relay",
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	}
	session := &peerSession{
		roomInfo: &RoomInfo{RoomID: "room-id"},
		groupID:  "group-id",
	}

	message, ok := localICECandidateMessage(session, "PUBLISHER", candidate)
	if !ok {
		t.Fatal("localICECandidateMessage ok = false, want true")
	}

	if got := stringValue(message, "roomId"); got != "room-id" {
		t.Fatalf("roomId = %q, want room-id", got)
	}
	if got := stringValue(message, "event"); got != "media-in" {
		t.Fatalf("event = %q, want media-in", got)
	}
	if got := stringValue(message, "groupId"); got != "group-id" {
		t.Fatalf("groupId = %q, want group-id", got)
	}

	payload := mapValue(message, "payload")
	if got := stringValue(payload, "method"); got != "rtc:ice" {
		t.Fatalf("payload.method = %q, want rtc:ice", got)
	}

	candidates := sliceValue(payload, "rtcIceCandidates")
	if len(candidates) != 1 {
		t.Fatalf("rtcIceCandidates length = %d, want 1", len(candidates))
	}
	gotCandidate, ok := candidates[0].(map[string]any)
	if !ok {
		t.Fatalf("rtcIceCandidates[0] type = %T, want map[string]any", candidates[0])
	}
	if got := stringValue(gotCandidate, "candidate"); got != candidate.Candidate {
		t.Fatalf("candidate = %q, want %q", got, candidate.Candidate)
	}
	if got := stringValue(gotCandidate, "sdpMid"); got != sdpMid {
		t.Fatalf("sdpMid = %q, want %q", got, sdpMid)
	}
	if got, ok := floatValue(gotCandidate, "sdpMLineIndex"); !ok || got != 0 {
		t.Fatalf("sdpMLineIndex = %v, %v, want 0, true", got, ok)
	}
	if got := stringValue(gotCandidate, "usernameFragment"); got != "" {
		t.Fatalf("usernameFragment = %q, want empty", got)
	}
	if got := stringValue(gotCandidate, "target"); got != "PUBLISHER" {
		t.Fatalf("target = %q, want PUBLISHER", got)
	}
}
