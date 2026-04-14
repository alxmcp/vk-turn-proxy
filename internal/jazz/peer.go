package jazz

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cacggghp/vk-turn-proxy/internal/namegen"
	"github.com/cacggghp/vk-turn-proxy/internal/rtcutil"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

const (
	dataChannelLabel          = "_reliable"
	maxDataChannelMessageSize = 12288
)

var debugLogging atomic.Bool

type Peer struct {
	roomInput string
	name      string
	onData    func([]byte)

	roomMu   sync.Mutex
	roomInfo *RoomInfo

	sessionMu sync.RWMutex
	session   *peerSession

	reconnectCbMu sync.RWMutex
	reconnectCb   func()
	wsMu          sync.Mutex
	sendMu        sync.Mutex
	reconnectCh   chan struct{}
	closeCh       chan struct{}
	closed        atomic.Bool
}

type peerSession struct {
	roomInfo  *RoomInfo
	ws        *websocket.Conn
	pcSub     *webrtc.PeerConnection
	pcPub     *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	groupID   string
	connected atomic.Bool
}

func NewPeer(roomInput, name string, onData func([]byte)) *Peer {
	return &Peer{
		roomInput:   roomInput,
		name:        name,
		onData:      onData,
		reconnectCh: make(chan struct{}, 1),
		closeCh:     make(chan struct{}),
	}
}

func NewConnectedPeer(ctx context.Context, roomInput string, onData func([]byte), onReconnect func()) (*Peer, error) {
	peer := NewPeer(roomInput, namegen.Generate(), onData)
	if onReconnect != nil {
		peer.SetReconnectCallback(onReconnect)
	}
	if err := peer.Connect(ctx); err != nil {
		if closeErr := peer.Close(); closeErr != nil {
			log.Printf("SaluteJazz DataChannel peer cleanup failed after connect error: %v", closeErr)
		}
		return nil, err
	}

	go peer.WatchConnection(ctx)

	return peer, nil
}

func SetDebug(enabled bool) {
	debugLogging.Store(enabled)
}

func DebugEnabled() bool {
	return debugLogging.Load()
}

func debugf(format string, args ...any) {
	if DebugEnabled() {
		log.Printf(format, args...)
	}
}

func (p *Peer) Connect(ctx context.Context) error {
	return p.connectOnce(ctx)
}

func (p *Peer) WatchConnection(ctx context.Context) {
	const (
		maxReconnects   = 10
		reconnectWindow = 5 * time.Minute
	)

	var lastReconnect time.Time
	reconnectCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.closeCh:
			return
		case <-p.reconnectCh:
		}

		now := time.Now()
		if now.Sub(lastReconnect) > reconnectWindow {
			reconnectCount = 0
		}
		if reconnectCount >= maxReconnects {
			log.Printf("SaluteJazz DataChannel: max reconnect attempts (%d) reached", maxReconnects)
			return
		}
		reconnectCount++
		lastReconnect = now

		backoff := time.Duration(reconnectCount) * 2 * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}

		for {
			if ctx.Err() != nil || p.closed.Load() {
				return
			}

			debugf("SaluteJazz DataChannel: reconnecting in %v", backoff)
			select {
			case <-ctx.Done():
				return
			case <-p.closeCh:
				return
			case <-time.After(backoff):
			}

			if err := p.reconnect(ctx); err != nil {
				debugf("SaluteJazz DataChannel: reconnect failed: %v", err)
				continue
			}

			debugf("SaluteJazz DataChannel: reconnected")
			break
		}
	}
}

func (p *Peer) Send(data []byte) error {
	if len(data) > maxDataChannelMessageSize {
		return fmt.Errorf("datachannel message too large: %d > %d", len(data), maxDataChannelMessageSize)
	}

	p.sessionMu.RLock()
	session := p.session
	var dc *webrtc.DataChannel
	if session != nil {
		dc = session.dc
	}
	p.sessionMu.RUnlock()

	p.sendMu.Lock()
	defer p.sendMu.Unlock()

	encoded := EncodeDataPacket(data)
	return rtcutil.SendDataChannel(dc, encoded)
}

func (p *Peer) Close() error {
	if p.closed.Swap(true) {
		return nil
	}

	select {
	case <-p.closeCh:
	default:
		close(p.closeCh)
	}

	p.cleanupCurrentSession()
	return nil
}

func (p *Peer) SetReconnectCallback(cb func()) {
	p.reconnectCbMu.Lock()
	p.reconnectCb = cb
	p.reconnectCbMu.Unlock()
}

func (p *Peer) reconnect(ctx context.Context) error {
	p.cleanupCurrentSession()
	if err := p.connectOnce(ctx); err != nil {
		return err
	}

	p.reconnectCbMu.RLock()
	cb := p.reconnectCb
	p.reconnectCbMu.RUnlock()
	if cb != nil {
		cb()
	}

	return nil
}

func (p *Peer) connectOnce(ctx context.Context) error {
	roomInfo, err := p.connectionInfo(ctx)
	if err != nil {
		return err
	}

	config := webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
		BundlePolicy: webrtc.BundlePolicyMaxBundle,
	}

	api := webrtc.NewAPI()

	pcSub, err := api.NewPeerConnection(config)
	if err != nil {
		return err
	}
	pcPub, err := api.NewPeerConnection(config)
	if err != nil {
		_ = pcSub.Close()
		return err
	}

	session := &peerSession{
		roomInfo: roomInfo,
		pcSub:    pcSub,
		pcPub:    pcPub,
	}

	pcSub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		debugf("SaluteJazz subscriber state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			p.signalReconnectIfCurrent(session, "subscriber state "+state.String())
		}
	})
	pcPub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		debugf("SaluteJazz publisher state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			p.signalReconnectIfCurrent(session, "publisher state "+state.String())
		}
	})

	dc, err := pcPub.CreateDataChannel(dataChannelLabel, &webrtc.DataChannelInit{
		Ordered: ptr(true),
	})
	if err != nil {
		p.cleanupSession(session)
		return err
	}
	session.dc = dc

	dcReady := make(chan struct{}, 1)
	dc.OnOpen(func() {
		if session.connected.CompareAndSwap(false, true) {
			log.Printf("SaluteJazz DataChannel connected")
		}
		select {
		case dcReady <- struct{}{}:
		default:
		}
	})
	dc.OnClose(func() {
		p.signalReconnectIfCurrent(session, "publisher datachannel closed")
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		p.handleIncomingMessage(msg.Data)
	})

	pcSub.OnDataChannel(func(remoteDC *webrtc.DataChannel) {
		if remoteDC.Label() != dataChannelLabel {
			debugf("SaluteJazz remote datachannel ignored: %s", remoteDC.Label())
			return
		}
		debugf("SaluteJazz remote datachannel opened: %s", remoteDC.Label())
		remoteDC.OnClose(func() {
			p.signalReconnectIfCurrent(session, "remote datachannel "+remoteDC.Label()+" closed")
		})
		remoteDC.OnMessage(func(msg webrtc.DataChannelMessage) {
			p.handleIncomingMessage(msg.Data)
		})
	})

	ws, resp, err := websocket.DefaultDialer.Dial(roomInfo.ConnectorURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		p.cleanupSession(session)
		return err
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	session.ws = ws

	if err := rtcutil.ConfigureWebSocketReadDeadline(ws, 60*time.Second); err != nil {
		p.cleanupSession(session)
		return err
	}

	p.sessionMu.Lock()
	p.session = session
	p.sessionMu.Unlock()

	go p.keepAlive(session)
	go p.handleSignaling(session)

	if err := p.sendJoin(session); err != nil {
		p.cleanupCurrentSession()
		return err
	}

	select {
	case <-ctx.Done():
		p.cleanupCurrentSession()
		return ctx.Err()
	case <-p.closeCh:
		p.cleanupCurrentSession()
		return context.Canceled
	case <-time.After(30 * time.Second):
		p.cleanupCurrentSession()
		return fmt.Errorf("datachannel timeout")
	case <-dcReady:
		return nil
	}
}

func (p *Peer) connectionInfo(ctx context.Context) (*RoomInfo, error) {
	p.roomMu.Lock()
	defer p.roomMu.Unlock()

	if p.roomInfo != nil {
		roomInfo, err := JoinRoom(ctx, p.roomInfo.RoomID+":"+p.roomInfo.Password)
		if err != nil {
			return nil, fmt.Errorf("refresh room connection: %w", err)
		}
		p.roomInfo.ConnectorURL = roomInfo.ConnectorURL
		return p.roomInfo, nil
	}

	roomID, _ := parseRoomInput(p.roomInput)
	if roomID == "" || roomID == "any" || roomID == "dummy" {
		roomInfo, err := CreateRoom(ctx)
		if err != nil {
			return nil, fmt.Errorf("create room: %w", err)
		}
		p.roomInfo = roomInfo
		log.Printf("SaluteJazz room created: %s:%s", roomInfo.RoomID, roomInfo.Password)
		return roomInfo, nil
	}

	roomInfo, err := JoinRoom(ctx, p.roomInput)
	if err != nil {
		return nil, fmt.Errorf("join room: %w", err)
	}
	p.roomInfo = roomInfo
	log.Printf("SaluteJazz joining room: %s", roomInfo.RoomID)
	return roomInfo, nil
}

func (p *Peer) keepAlive(session *peerSession) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.closeCh:
			return
		case <-ticker.C:
			if err := p.writeControl(session, websocket.PingMessage, []byte{}); err != nil {
				p.signalReconnectIfCurrent(session, "websocket ping failed")
				return
			}
		}
	}
}

func (p *Peer) handleIncomingMessage(data []byte) {
	payload, ok := DecodeDataPacket(data)
	if !ok {
		debugf("SaluteJazz DataChannel: failed to decode packet, using raw payload")
		payload = data
	}

	if p.onData != nil && len(payload) > 0 {
		p.onData(payload)
	}
}

func (p *Peer) handleSignaling(session *peerSession) {
	for {
		var msg map[string]any
		if err := session.ws.ReadJSON(&msg); err != nil {
			if p.isCurrentSession(session) && !p.closed.Load() {
				debugf("SaluteJazz signaling read error: %v", err)
				p.signalReconnectIfCurrent(session, "signaling read failed")
			}
			return
		}
		if err := session.ws.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			if p.isCurrentSession(session) && !p.closed.Load() {
				debugf("SaluteJazz signaling deadline update failed: %v", err)
				p.signalReconnectIfCurrent(session, "signaling deadline update failed")
			}
			return
		}

		event := stringValue(msg, "event")
		payload := mapValue(msg, "payload")

		switch event {
		case "join-response":
			p.handleJoinResponse(session, payload)
		case "media-out":
			p.handleMediaOut(session, payload)
		}
	}
}

func (p *Peer) handleJoinResponse(session *peerSession, payload map[string]any) {
	group := mapValue(payload, "participantGroup")
	session.groupID = stringValue(group, "groupId")
	debugf("SaluteJazz peer joined: groupId=%s", session.groupID)
}

func (p *Peer) handleMediaOut(session *peerSession, payload map[string]any) {
	method := stringValue(payload, "method")

	switch method {
	case "rtc:config":
		p.handleRTCConfig(session, payload)
	case "rtc:offer":
		p.handleSubscriberOffer(session, payload)
	case "rtc:answer":
		p.handlePublisherAnswer(session, payload)
	case "rtc:ice":
		p.handleICE(session, payload)
	}
}

func (p *Peer) handleRTCConfig(session *peerSession, payload map[string]any) {
	config := mapValue(payload, "configuration")
	servers := sliceValue(config, "iceServers")

	iceServers := make([]webrtc.ICEServer, 0, len(servers))
	for _, rawServer := range servers {
		server, ok := rawServer.(map[string]any)
		if !ok {
			continue
		}
		rawURLs := sliceValue(server, "urls")
		username := stringValue(server, "username")
		credential := stringValue(server, "credential")

		urls := make([]string, 0, len(rawURLs))
		for _, rawURL := range rawURLs {
			if url, ok := rawURL.(string); ok && url != "" {
				urls = append(urls, url)
			}
		}
		if len(urls) > 0 {
			iceServers = append(iceServers, webrtc.ICEServer{
				URLs:       urls,
				Username:   username,
				Credential: credential,
			})
		}
	}

	if len(iceServers) == 0 {
		return
	}

	newConfig := webrtc.Configuration{
		ICEServers:   iceServers,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
		BundlePolicy: webrtc.BundlePolicyMaxBundle,
	}
	if err := session.pcSub.SetConfiguration(newConfig); err != nil {
		debugf("SaluteJazz subscriber SetConfiguration failed: %v", err)
	}
	if err := session.pcPub.SetConfiguration(newConfig); err != nil {
		debugf("SaluteJazz publisher SetConfiguration failed: %v", err)
	}
}

func (p *Peer) handleSubscriberOffer(session *peerSession, payload map[string]any) {
	desc := mapValue(payload, "description")
	sdp := stringValue(desc, "sdp")
	if sdp == "" {
		debugf("SaluteJazz subscriber offer missing SDP")
		return
	}

	if err := session.pcSub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		debugf("SaluteJazz subscriber SetRemoteDescription failed: %v", err)
		return
	}

	answer, err := session.pcSub.CreateAnswer(nil)
	if err != nil {
		debugf("SaluteJazz subscriber CreateAnswer failed: %v", err)
		return
	}
	if err := session.pcSub.SetLocalDescription(answer); err != nil {
		debugf("SaluteJazz subscriber SetLocalDescription failed: %v", err)
		return
	}

	if err := p.writeJSON(session, map[string]any{
		"roomId":    session.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   session.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:answer",
			"description": map[string]any{
				"type": "answer",
				"sdp":  answer.SDP,
			},
		},
	}); err != nil {
		debugf("SaluteJazz subscriber answer send failed: %v", err)
		return
	}

	time.Sleep(300 * time.Millisecond)
	p.sendPublisherOffer(session)
}

func (p *Peer) sendPublisherOffer(session *peerSession) {
	offer, err := session.pcPub.CreateOffer(nil)
	if err != nil {
		debugf("SaluteJazz publisher CreateOffer failed: %v", err)
		return
	}

	if err := session.pcPub.SetLocalDescription(offer); err != nil {
		debugf("SaluteJazz publisher SetLocalDescription failed: %v", err)
		return
	}

	if err := p.writeJSON(session, map[string]any{
		"roomId":    session.roomInfo.RoomID,
		"event":     "media-in",
		"groupId":   session.groupID,
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"method": "rtc:offer",
			"description": map[string]any{
				"type": "offer",
				"sdp":  offer.SDP,
			},
		},
	}); err != nil {
		debugf("SaluteJazz publisher offer send failed: %v", err)
	}
}

func (p *Peer) handlePublisherAnswer(session *peerSession, payload map[string]any) {
	desc := mapValue(payload, "description")
	sdp := stringValue(desc, "sdp")
	if sdp == "" {
		debugf("SaluteJazz publisher answer missing SDP")
		return
	}
	if err := session.pcPub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		debugf("SaluteJazz publisher SetRemoteDescription failed: %v", err)
	}
}

func (p *Peer) handleICE(session *peerSession, payload map[string]any) {
	candidates := sliceValue(payload, "rtcIceCandidates")

	for _, rawCandidate := range candidates {
		cand, ok := rawCandidate.(map[string]any)
		if !ok {
			continue
		}
		candidate := stringValue(cand, "candidate")
		target := stringValue(cand, "target")
		sdpMid := stringValue(cand, "sdpMid")
		sdpMLineIndex, ok := floatValue(cand, "sdpMLineIndex")
		if candidate == "" || target == "" || !ok {
			continue
		}

		index := uint16(sdpMLineIndex)
		init := webrtc.ICECandidateInit{
			Candidate:     candidate,
			SDPMid:        &sdpMid,
			SDPMLineIndex: &index,
		}

		switch target {
		case "SUBSCRIBER":
			if err := session.pcSub.AddICECandidate(init); err != nil {
				debugf("SaluteJazz subscriber ICE apply failed: %v", err)
			}
		case "PUBLISHER":
			if err := session.pcPub.AddICECandidate(init); err != nil {
				debugf("SaluteJazz publisher ICE apply failed: %v", err)
			}
		}
	}
}

func (p *Peer) sendJoin(session *peerSession) error {
	return p.writeJSON(session, map[string]any{
		"roomId":    session.roomInfo.RoomID,
		"event":     "join",
		"requestId": uuid.New().String(),
		"payload": map[string]any{
			"password":        session.roomInfo.Password,
			"participantName": p.name,
			"supportedFeatures": map[string]any{
				"attachedRooms": true,
				"sessionGroups": true,
				"transcription": true,
			},
			"isSilent": false,
		},
	})
}

func (p *Peer) writeJSON(session *peerSession, payload any) error {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()

	if session.ws == nil {
		return fmt.Errorf("websocket is closed")
	}

	return session.ws.WriteJSON(payload)
}

func (p *Peer) writeControl(session *peerSession, messageType int, data []byte) error {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()

	if session.ws == nil {
		return fmt.Errorf("websocket is closed")
	}

	return session.ws.WriteControl(messageType, data, time.Now().Add(10*time.Second))
}

func (p *Peer) cleanupCurrentSession() {
	p.sessionMu.Lock()
	session := p.session
	p.session = nil
	p.sessionMu.Unlock()

	p.cleanupSession(session)
}

func (p *Peer) cleanupSession(session *peerSession) {
	if session == nil {
		return
	}

	if session.connected.Swap(false) {
		log.Printf("SaluteJazz DataChannel closed")
	}

	if session.dc != nil {
		_ = session.dc.Close()
	}
	if session.pcPub != nil {
		_ = session.pcPub.Close()
	}
	if session.pcSub != nil {
		_ = session.pcSub.Close()
	}
	if session.ws != nil {
		p.wsMu.Lock()
		if err := session.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)); err != nil {
			debugf("SaluteJazz websocket close control failed: %v", err)
		}
		_ = session.ws.Close()
		p.wsMu.Unlock()
	}
}

func (p *Peer) isCurrentSession(session *peerSession) bool {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	return p.session == session
}

func (p *Peer) signalReconnectIfCurrent(session *peerSession, reason string) {
	if p.closed.Load() || !p.isCurrentSession(session) {
		return
	}

	if session.connected.Swap(false) {
		log.Printf("SaluteJazz DataChannel closed")
	}
	if reason != "" {
		debugf("SaluteJazz DataChannel disconnect reason: %s", reason)
	}

	select {
	case p.reconnectCh <- struct{}{}:
	default:
	}
}

func ptr[T any](value T) *T {
	return &value
}

func stringValue(values map[string]any, key string) string {
	value, ok := values[key].(string)
	if !ok {
		return ""
	}
	return value
}

func mapValue(values map[string]any, key string) map[string]any {
	value, ok := values[key].(map[string]any)
	if !ok {
		return nil
	}
	return value
}

func sliceValue(values map[string]any, key string) []any {
	value, ok := values[key].([]any)
	if !ok {
		return nil
	}
	return value
}

func floatValue(values map[string]any, key string) (float64, bool) {
	value, ok := values[key].(float64)
	return value, ok
}
