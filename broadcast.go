package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
)

type DistributionFunc func([]string, []uuid.UUID) map[uuid.UUID]map[string]bool

func AllDist(senders []string, receivers []uuid.UUID) map[uuid.UUID]map[string]bool {
	outputMap := make(map[uuid.UUID]map[string]bool)
	for _, receiver := range receivers {
		sendersMap := make(map[string]bool)
		for _, sender := range senders {
			sendersMap[sender] = true
		}
		outputMap[receiver] = sendersMap
	}
	return outputMap
}

func RRDist(senders []string, receivers []uuid.UUID) map[uuid.UUID]map[string]bool {
	outputMap := make(map[uuid.UUID]map[string]bool)
	if len(receivers) == 0 {
		return outputMap
	}
	for _, receiver := range receivers {
		outputMap[receiver] = make(map[string]bool)
	}
	for i, sender := range senders {
		outputMap[receivers[i%len(receivers)]][sender] = true
	}
	return outputMap
}

type Broadcaster struct {
	peerSender map[uuid.UUID]PeerSenderState
	senders    map[string]webrtc.TrackLocal
	receivers  map[uuid.UUID]ReceiverState
	lock       sync.Mutex

	distributionFunction DistributionFunc
}

type PeerSenderState struct {
	ETag     string
	PeerConn *webrtc.PeerConnection
}

func NewBroadcaster(distFunc DistributionFunc) Broadcaster {
	return Broadcaster{
		distributionFunction: distFunc,
		senders:              make(map[string]webrtc.TrackLocal),
		receivers:            make(map[uuid.UUID]ReceiverState),
		peerSender:           make(map[uuid.UUID]PeerSenderState),
	}
}

func (s *Broadcaster) AddPeerSender(peer PeerSenderState) uuid.UUID {
	s.lock.Lock()
	defer s.lock.Unlock()
	id := uuid.New()
	s.peerSender[id] = peer
	return id
}
func (s *Broadcaster) DeletePeerSender(id uuid.UUID) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.receivers, id)
}
func (s *Broadcaster) GetPeerSender(id uuid.UUID) (PeerSenderState, bool) {
	v, ok := s.peerSender[id]
	return v, ok
}

func (s *Broadcaster) AddSender(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	s.lock.Lock()
	defer s.lock.Unlock()

	trackLocal, err := webrtc.NewTrackLocalStaticRTP(
		t.Codec().RTPCodecCapability,
		t.ID(),
		t.StreamID(),
	)
	if err != nil {
		zap.S().Errorw("Unable to create local track", "trackID", t.ID(), "streamID", t.StreamID())
		return nil
	}

	s.senders[trackLocal.StreamID()+trackLocal.ID()] = trackLocal
	zap.S().Debugw("Add new track", "TrackID", t.ID(), "TrackStreamID", t.StreamID())
	go func() {
		buf := make([]byte, 1500)
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				s.RemoveSender(trackLocal)
				return
			}

			if _, err = trackLocal.Write(buf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				return
			}
		}
	}()
	go s.rebalanceReceivers()

	return trackLocal
}

func (s *Broadcaster) AddReceiver(receiver ReceiverState) uuid.UUID {
	s.lock.Lock()
	defer s.lock.Unlock()

	id := uuid.New()

	s.receivers[id] = receiver
	go s.rebalanceReceivers()

	return id
}

func (s *Broadcaster) RemoveSender(t webrtc.TrackLocal) {
	s.lock.Lock()
	defer s.lock.Unlock()
	zap.S().Debugw("Removing Track", "StreamID", t.StreamID(), "TrackID", t.ID())

	_, ok := s.senders[t.StreamID()+t.ID()]
	if !ok {
		return
	}

	delete(s.senders, t.StreamID()+t.ID())
	go s.rebalanceReceivers()
}

func (s *Broadcaster) RemoveReceiver(id uuid.UUID) {
	s.lock.Lock()
	defer s.lock.Unlock()

	receiver, ok := s.receivers[id]
	if !ok {
		return
	}

	receiver.SignalSocket.Close(websocket.StatusNormalClosure, "Ending operation")
	receiver.Connection.Close()

	delete(s.receivers, id)
	go s.rebalanceReceivers()
}

func (s *Broadcaster) pruneClosedConnections() {
	for u, rs := range s.receivers {
		if rs.Connection.ConnectionState() == webrtc.PeerConnectionStateClosed {
			rs.SignalSocket.Close(websocket.StatusGoingAway, "WebRTC connection closed")
			delete(s.receivers, u)
		}
	}
}

func (s *Broadcaster) rebalanceReceivers() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.pruneClosedConnections()

	receivers := make([]uuid.UUID, 0, len(s.receivers))
	for u := range s.receivers {
		receivers = append(receivers, u)
	}
	senders := make([]string, 0, len(s.senders))
	for u := range s.senders {
		senders = append(senders, u)
	}
	match := s.distributionFunction(senders, receivers)
	for u, v := range match {
		receiver := s.receivers[u]
		existingSenders := make(map[string]bool)
		for _, sender := range receiver.Connection.GetSenders() {
			if sender.Track() == nil {
				continue
			}

			existingSenders[sender.Track().StreamID()+sender.Track().ID()] = true

			if _, ok := v[sender.Track().StreamID()+sender.Track().ID()]; !ok {
				receiver.Connection.RemoveTrack(sender)
			}
		}

		for trackID := range v {
			if _, ok := existingSenders[trackID]; !ok {
				receiver.Connection.AddTrack(s.senders[trackID])
			}
		}

		offer, err := receiver.Connection.CreateOffer(nil)
		if err != nil {
			zap.S().Errorw("Unable to create offer", "receiver", u)
			continue
		}

		err = receiver.Connection.SetLocalDescription(offer)
		if err != nil {
			zap.S().Error(err)
		}

		zap.S().Debugw("Sending offer", "offer", offer)
		offerString, err := json.Marshal(offer)
		if err != nil {
			zap.S().Errorw("Unable to marshal offer to json", "receiver", u, "offer", offer)
			continue
		}
		message := websocketMessage{
			Event: "offer",
			Data:  string(offerString),
		}
		messageString, err := json.Marshal(message)
		if err != nil {
			zap.S().Errorw("Unable to marshal offer to json", "receiver", u, "offer", offer)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		receiver.SignalSocket.Write(ctx, websocket.MessageText, messageString)
	}
}

type ReceiverState struct {
	Connection   *webrtc.PeerConnection
	SignalSocket *websocket.Conn
}

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}
