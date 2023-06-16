package main

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

func whipHandler(b *Broadcaster) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		logger := r.Context().Value(LOGGER).(*zap.SugaredLogger)
		if r.Header.Get("content-type") != "application/sdp" {
			http.Error(w, "Unsupported content type", http.StatusNotAcceptable)
			return
		}
		boffer, err := io.ReadAll(r.Body)
		if err != nil {
			logger.Error(err)
			return
		}
		offer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  string(boffer),
		}

		peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			logger.Error(err)
		}

		if _, err = peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
			logger.Error(err)
		}

		peer.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
			// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it
			go func() {
				ticker := time.NewTicker(3 * time.Second)
				for range ticker.C {
					if rtcpSendErr := peer.WriteRTCP(
						[]rtcp.Packet{
							&rtcp.PictureLossIndication{
								MediaSSRC: uint32(remoteTrack.SSRC()),
							}},
					); rtcpSendErr != nil {
						logger.Info(rtcpSendErr)
						return
					}
				}
			}()

			b.AddSender(remoteTrack)
		})
		// Set the remote SessionDescription
		err = peer.SetRemoteDescription(offer)
		if err != nil {
			panic(err)
		}
		gatherComplete := webrtc.GatheringCompletePromise(peer)

		// Create answer
		answer, err := peer.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		if err := peer.SetLocalDescription(answer); err != nil {
			panic(err)
		}

		<-gatherComplete

		senderState := PeerSenderState{
			PeerConn: peer,
			ETag:     uuid.NewString(),
		}
		peerID := b.AddPeerSender(senderState)
		w.Header().Add("content-type", "application/sdp")
		w.Header().Add("Location", fmt.Sprintf("/whip/%s", peerID.String()))
		w.Header().Add("ETag", fmt.Sprintf("\"%s\"", senderState.ETag))
		w.Header().Add("Accept-Patch", "application/trickle-ice-sdpfrag")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(peer.LocalDescription().SDP))
	}
}

func whipDeleteHandler(b *Broadcaster) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		logger := r.Context().Value(LOGGER).(*zap.SugaredLogger)
		peerID, err := uuid.Parse(chi.URLParam(r, "peerID"))
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		peer, ok := b.GetPeerSender(peerID)
		if !ok {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		if peer.PeerConn.Close() != nil {
			logger.Error("Unable to close peer connection")
			http.Error(w, "Error closing peer connection", http.StatusInternalServerError)
			return
		}
		b.DeletePeerSender(peerID)
		w.WriteHeader(http.StatusOK)
	}
}
