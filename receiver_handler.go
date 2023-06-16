package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
)

func webSocketHandler(b *Broadcaster) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		logger := r.Context().Value(LOGGER).(*zap.SugaredLogger)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"webRTCBroadcast"},
		})
		if err != nil {
			logger.Errorw("Failed to upgrade", "error", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "the sky is falling")

		peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			logger.Errorw("Failed to create PeerConnection", "error", err)
			return
		}

		// When this frame returns close the PeerConnection
		defer peerConnection.Close()
		state := ReceiverState{
			Connection:   peerConnection,
			SignalSocket: c,
		}

		dc, err := peerConnection.CreateDataChannel("ping", nil)
		if err != nil {
			logger.Error(err)
		}
		defer dc.Close()
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			for range ticker.C {
				if err := dc.SendText("ping"); err != nil {
					return
				}
			}
		}()

		// Trickle ICE. Emit server candidate to client
		peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
			if i == nil {
				return
			}

			candidateString, err := json.Marshal(i.ToJSON())
			if err != nil {
				logger.Errorw("Unable to marshal to json", "error", err, "candidate", i)
				return
			}

			message := websocketMessage{
				Event: "candidate",
				Data:  string(candidateString),
			}
			messageString, err := json.Marshal(message)
			if err != nil {
				logger.Error(err)
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if writeErr := c.Write(ctx, websocket.MessageText, messageString); writeErr != nil {
				logger.Errorw("Unable to write to ws", "error", writeErr)
			}
		})
		receiverID := b.AddReceiver(state)

		// If PeerConnection is closed remove it from global list
		peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
			switch p {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				if err := peerConnection.Close(); err != nil {
					logger.Errorw("Unable to close connection", "error", err)
				}
				b.RemoveReceiver(receiverID)
			}
		})

		message := &websocketMessage{}
		for {
			_, raw, err := c.Read(r.Context())
			if err != nil {
				if websocket.CloseStatus(err) == websocket.StatusGoingAway {
					return
				}
				logger.Error(err)
				return
			} else if err := json.Unmarshal(raw, &message); err != nil {
				logger.Error(err)
				return
			}

			logger.Debugw("Received message", "message", message)

			switch message.Event {
			case "candidate":
				candidate := webrtc.ICECandidateInit{}
				if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
					logger.Error(err)
					return
				}

				if err := peerConnection.AddICECandidate(candidate); err != nil {
					logger.Error(err)
					return
				}
			case "answer":
				answer := webrtc.SessionDescription{}
				if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
					logger.Error(err)
					return
				}

				if err := peerConnection.SetRemoteDescription(answer); err != nil && err != webrtc.ErrSessionDescriptionMissingIceUfrag {
					logger.Error(err)
					return
				}
			}
		}
	}
}
