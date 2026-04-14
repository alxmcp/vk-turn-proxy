package telemost

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
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
	dataChannelLabel = "vk-turn-proxy"
)

var debugLogging atomic.Bool

type Peer struct {
	roomURL string
	name    string
	onData  func([]byte)

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
	conn      *ConnectionInfo
	ws        *websocket.Conn
	pcSub     *webrtc.PeerConnection
	pcPub     *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	connected atomic.Bool
}

func NewPeer(roomURL, name string, onData func([]byte)) *Peer {
	return &Peer{
		roomURL:     roomURL,
		name:        name,
		onData:      onData,
		reconnectCh: make(chan struct{}, 1),
		closeCh:     make(chan struct{}),
	}
}

// NewConnectedPeer parses a Telemost invite link, creates a peer, connects it,
// and starts the background reconnect watcher.
func NewConnectedPeer(ctx context.Context, inviteLink string, onData func([]byte), onReconnect func()) (*Peer, error) {
	target, err := ParseRoomTarget(inviteLink)
	if err != nil {
		return nil, err
	}

	peer := NewPeer(target.RoomID, namegen.Generate(), onData)
	if onReconnect != nil {
		peer.SetReconnectCallback(onReconnect)
	}
	if err := peer.Connect(ctx); err != nil {
		if closeErr := peer.Close(); closeErr != nil {
			log.Printf("Telemost DataChannel peer cleanup failed after connect error: %v", closeErr)
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

func isTransportDataChannelLabel(label string) bool {
	return label == dataChannelLabel
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
			log.Printf("Telemost DataChannel: max reconnect attempts (%d) reached", maxReconnects)
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

			debugf("Telemost DataChannel: reconnecting in %v", backoff)
			select {
			case <-ctx.Done():
				return
			case <-p.closeCh:
				return
			case <-time.After(backoff):
			}

			if err := p.reconnect(ctx); err != nil {
				debugf("Telemost DataChannel: reconnect failed: %v", err)
				continue
			}

			debugf("Telemost DataChannel: reconnected")
			break
		}
	}
}

func (p *Peer) Send(data []byte) error {
	p.sessionMu.RLock()
	session := p.session
	var dc *webrtc.DataChannel
	if session != nil {
		dc = session.dc
	}
	p.sessionMu.RUnlock()

	p.sendMu.Lock()
	defer p.sendMu.Unlock()

	return rtcutil.SendDataChannel(dc, data)
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
	conn, roomURL, err := GetConnectionInfo(ctx, p.roomURL, p.name)
	if err != nil {
		return err
	}
	origin := WebOriginFromRoomURL(roomURL)

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.rtc.yandex.net:3478"}},
		},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
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
		conn:  conn,
		pcSub: pcSub,
		pcPub: pcPub,
	}

	pcSub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		debugf("Telemost subscriber state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			p.signalReconnectIfCurrent(session, "subscriber state "+state.String())
		}
	})
	pcPub.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		debugf("Telemost publisher state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			p.signalReconnectIfCurrent(session, "publisher state "+state.String())
		}
	})

	dc, err := pcPub.CreateDataChannel(dataChannelLabel, nil)
	if err != nil {
		p.cleanupSession(session)
		return err
	}
	session.dc = dc

	dcReady := make(chan struct{}, 1)
	dc.OnOpen(func() {
		if session.connected.CompareAndSwap(false, true) {
			log.Printf("Telemost DataChannel connected")
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
		if p.onData != nil && len(msg.Data) > 0 {
			p.onData(msg.Data)
		}
	})

	pcSub.OnDataChannel(func(remoteDC *webrtc.DataChannel) {
		if !isTransportDataChannelLabel(remoteDC.Label()) {
			debugf("Telemost remote datachannel ignored: %s", remoteDC.Label())
			return
		}
		debugf("Telemost remote datachannel opened: %s", remoteDC.Label())
		remoteDC.OnClose(func() {
			p.signalReconnectIfCurrent(session, "remote datachannel "+remoteDC.Label()+" closed")
		})
		remoteDC.OnMessage(func(msg webrtc.DataChannelMessage) {
			if p.onData != nil && len(msg.Data) > 0 {
				p.onData(msg.Data)
			}
		})
	})

	headers := http.Header{}
	headers.Set("Origin", origin)
	headers.Set("Referer", origin+"/")

	ws, resp, err := websocket.DefaultDialer.Dial(conn.ClientConfig.MediaServerURL, headers)
	if err != nil {
		if resp != nil && resp.Body != nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				debugf("Telemost websocket response close failed: %v", closeErr)
			}
		}
		p.cleanupSession(session)
		return err
	}
	session.ws = ws

	if err := rtcutil.ConfigureWebSocketReadDeadline(ws, 60*time.Second); err != nil {
		p.cleanupSession(session)
		return err
	}

	p.sessionMu.Lock()
	p.session = session
	p.sessionMu.Unlock()

	p.setupICEHandlers(session)

	go p.keepAlive(session)
	go p.handleSignaling(session)

	if err := p.sendHello(session); err != nil {
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
	case <-time.After(15 * time.Second):
		p.cleanupCurrentSession()
		return fmt.Errorf("datachannel timeout")
	case <-dcReady:
		return nil
	}
}

func (p *Peer) keepAlive(session *peerSession) {
	wsPingTicker := time.NewTicker(30 * time.Second)
	defer wsPingTicker.Stop()
	appPingTicker := time.NewTicker(5 * time.Second)
	defer appPingTicker.Stop()

	for {
		select {
		case <-p.closeCh:
			return
		case <-wsPingTicker.C:
			if err := p.writeControl(session, websocket.PingMessage, []byte{}); err != nil {
				p.signalReconnectIfCurrent(session, "websocket ping failed")
				return
			}
		case <-appPingTicker.C:
			if err := p.writeJSON(session, map[string]interface{}{
				"uid":  uuid.New().String(),
				"ping": map[string]interface{}{},
			}); err != nil {
				p.signalReconnectIfCurrent(session, "application ping failed")
				return
			}
		}
	}
}

func (p *Peer) handleSignaling(session *peerSession) {
	pubSent := false

	for {
		var msg map[string]interface{}
		if err := session.ws.ReadJSON(&msg); err != nil {
			if p.isCurrentSession(session) && !p.closed.Load() {
				debugf("Telemost signaling read error: %v", err)
				p.signalReconnectIfCurrent(session, "signaling read failed")
			}
			return
		}
		if err := session.ws.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			if p.isCurrentSession(session) && !p.closed.Load() {
				debugf("Telemost signaling read deadline update failed: %v", err)
				p.signalReconnectIfCurrent(session, "signaling deadline update failed")
			}
			return
		}

		uid := ""
		if rawUID, ok := msg["uid"].(string); ok {
			uid = rawUID
		}

		sendAck := func() {
			if err := p.sendAck(session, uid); err != nil {
				debugf("Telemost signaling ack failed: %v", err)
			}
		}

		if _, ok := msg["serverHello"]; ok {
			sendAck()
		}
		if _, ok := msg["updateDescription"]; ok {
			sendAck()
		}
		if _, ok := msg["vadActivity"]; ok {
			sendAck()
		}
		if _, ok := msg["ping"]; ok {
			if err := p.sendPong(session, uid); err != nil {
				debugf("Telemost signaling pong failed: %v", err)
			}
			continue
		}
		if _, ok := msg["pong"]; ok {
			sendAck()
			continue
		}

		if offer, ok := msg["subscriberSdpOffer"].(map[string]interface{}); ok && !pubSent {
			sdp, ok := offer["sdp"].(string)
			if !ok || sdp == "" {
				debugf("Telemost subscriber offer missing SDP")
				continue
			}

			pcSeq, ok := offer["pcSeq"].(float64)
			if !ok {
				debugf("Telemost subscriber offer missing pcSeq")
				continue
			}

			if err := session.pcSub.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  sdp,
			}); err != nil {
				debugf("Telemost subscriber SetRemoteDescription failed: %v", err)
				continue
			}

			answer, err := session.pcSub.CreateAnswer(nil)
			if err != nil {
				debugf("Telemost subscriber CreateAnswer failed: %v", err)
				continue
			}
			if err := session.pcSub.SetLocalDescription(answer); err != nil {
				debugf("Telemost subscriber SetLocalDescription failed: %v", err)
				continue
			}
			if err := p.writeJSON(session, map[string]interface{}{
				"uid": uuid.New().String(),
				"subscriberSdpAnswer": map[string]interface{}{
					"pcSeq": int(pcSeq),
					"sdp":   answer.SDP,
				},
			}); err != nil {
				debugf("Telemost subscriber answer send failed: %v", err)
				continue
			}

			sendAck()
			time.Sleep(300 * time.Millisecond)

			pubOffer, err := session.pcPub.CreateOffer(nil)
			if err != nil {
				debugf("Telemost publisher CreateOffer failed: %v", err)
				continue
			}
			if err := session.pcPub.SetLocalDescription(pubOffer); err != nil {
				debugf("Telemost publisher SetLocalDescription failed: %v", err)
				continue
			}
			if err := p.writeJSON(session, map[string]interface{}{
				"uid": uuid.New().String(),
				"publisherSdpOffer": map[string]interface{}{
					"pcSeq": 1,
					"sdp":   pubOffer.SDP,
				},
			}); err != nil {
				debugf("Telemost publisher offer send failed: %v", err)
				continue
			}

			pubSent = true
			continue
		}

		if answer, ok := msg["publisherSdpAnswer"].(map[string]interface{}); ok {
			sdp, ok := answer["sdp"].(string)
			if !ok || sdp == "" {
				debugf("Telemost publisher answer missing SDP")
				continue
			}
			if err := session.pcPub.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  sdp,
			}); err != nil {
				debugf("Telemost publisher SetRemoteDescription failed: %v", err)
			}
			sendAck()
			continue
		}

		if cand, ok := msg["webrtcIceCandidate"].(map[string]interface{}); ok {
			p.handleICE(session, cand)
		}
	}
}

func (p *Peer) setupICEHandlers(session *peerSession) {
	session.pcSub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		if err := p.writeJSON(session, map[string]interface{}{
			"uid": uuid.New().String(),
			"webrtcIceCandidate": map[string]interface{}{
				"candidate":     init.Candidate,
				"sdpMid":        init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":        "SUBSCRIBER",
				"pcSeq":         1,
			},
		}); err != nil && p.isCurrentSession(session) && !p.closed.Load() {
			debugf("Telemost subscriber ICE send failed: %v", err)
		}
	})

	session.pcPub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		if err := p.writeJSON(session, map[string]interface{}{
			"uid": uuid.New().String(),
			"webrtcIceCandidate": map[string]interface{}{
				"candidate":     init.Candidate,
				"sdpMid":        init.SDPMid,
				"sdpMlineIndex": init.SDPMLineIndex,
				"target":        "PUBLISHER",
				"pcSeq":         1,
			},
		}); err != nil && p.isCurrentSession(session) && !p.closed.Load() {
			debugf("Telemost publisher ICE send failed: %v", err)
		}
	})
}

func (p *Peer) handleICE(session *peerSession, cand map[string]interface{}) {
	candidate, ok := cand["candidate"].(string)
	if !ok || candidate == "" || len(strings.Fields(candidate)) < 8 {
		return
	}

	target, ok := cand["target"].(string)
	if !ok || target == "" {
		return
	}

	sdpMid := ""
	if rawSDPMid, ok := cand["sdpMid"].(string); ok {
		sdpMid = rawSDPMid
	}
	sdpMLineIndex, ok := cand["sdpMlineIndex"].(float64)
	if !ok {
		return
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
			debugf("Telemost subscriber ICE apply failed: %v", err)
		}
	case "PUBLISHER":
		if err := session.pcPub.AddICECandidate(init); err != nil {
			debugf("Telemost publisher ICE apply failed: %v", err)
		}
	}
}

func (p *Peer) sendHello(session *peerSession) error {
	hello := map[string]interface{}{
		"uid": uuid.New().String(),
		"hello": map[string]interface{}{
			"participantMeta": map[string]interface{}{
				"name":      p.name,
				"role":      "SPEAKER",
				"sendAudio": false,
				"sendVideo": false,
			},
			"participantAttributes": map[string]interface{}{
				"name": p.name,
				"role": "SPEAKER",
			},
			"sendAudio":     false,
			"sendVideo":     false,
			"sendSharing":   false,
			"participantId": session.conn.PeerID,
			"roomId":        session.conn.RoomID,
			"serviceName":   "telemost",
			"credentials":   session.conn.Credentials,
			"capabilitiesOffer": map[string]interface{}{
				"offerAnswerMode":        []string{"SEPARATE"},
				"initialSubscriberOffer": []string{"ON_HELLO"},
				"slotsMode":              []string{"FROM_CONTROLLER"},
				"simulcastMode":          []string{"DISABLED"},
				"selfVadStatus":          []string{"FROM_SERVER"},
				"dataChannelSharing":     []string{"TO_RTP"},
			},
			"sdkInfo": map[string]interface{}{
				"implementation": "go",
				"version":        "1.0.0",
				"userAgent":      "vk-turn-proxy-" + p.name,
			},
			"sdkInitializationId": uuid.New().String(),
			"disablePublisher":    false,
			"disableSubscriber":   false,
		},
	}

	return p.writeJSON(session, hello)
}

func (p *Peer) sendAck(session *peerSession, uid string) error {
	return p.writeJSON(session, map[string]interface{}{
		"uid": uid,
		"ack": map[string]interface{}{
			"status": map[string]interface{}{
				"code": "OK",
			},
		},
	})
}

func (p *Peer) sendPong(session *peerSession, uid string) error {
	return p.writeJSON(session, map[string]interface{}{
		"uid":  uid,
		"pong": map[string]interface{}{},
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
		log.Printf("Telemost DataChannel closed")
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
			debugf("Telemost websocket close control failed: %v", err)
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
		log.Printf("Telemost DataChannel closed")
	}
	if reason != "" {
		debugf("Telemost DataChannel disconnect reason: %s", reason)
	}

	select {
	case p.reconnectCh <- struct{}{}:
	default:
	}
}
