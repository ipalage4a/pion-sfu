package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

var (
	addr     = flag.String("addr", ":8080", "http service address")
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	indexTemplate = &template.Template{}

	// lock for peerConnections and trackLocals
	listLock        sync.RWMutex
	peerConnections = make(map[int]*peerConnectionState)
	trackLocals     map[string]*webrtc.TrackLocalStaticRTP
)

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
	Id    int    `json:"id"`
}

type peerConnectionState struct {
	*webrtc.PeerConnection
	websocket     *threadSafeWriter
	mux           sync.Mutex
	answerPending bool
	negoPengind   bool
}

func main() {
	// Parse the flags passed to program
	flag.Parse()

	// Init other state
	log.SetFlags(0)
	trackLocals = map[string]*webrtc.TrackLocalStaticRTP{}

	// Read index.html from disk into memory, serve whenever anyone requests /
	indexHTML, err := ioutil.ReadFile("index.html")
	if err != nil {
		panic(err)
	}
	indexTemplate = template.Must(template.New("").Parse(string(indexHTML)))

	// websocket handler
	http.HandleFunc("/websocket", websocketHandler)

	// index.html handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			log.Fatal(err)
		}
	})

	// request a keyframe every 3 seconds
	go func() {
		for range time.NewTicker(time.Second * 3).C {
			dispatchKeyFrame()
		}
	}()

	// start HTTP server
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// Add to list of tracks and fire renegotation for all PeerConnections
func addTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		signalPeerConnections("add track")
	}()

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	trackLocals[t.ID()] = trackLocal
	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections
func removeTrack(t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		signalPeerConnections("remove track")
	}()

	delete(trackLocals, t.ID())
}

// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks
func signalPeerConnections(reason string) {
	log.Println("signal reason: ", reason)

	attemptSync := func() (tryAgain bool) {
		listLock.Lock()
		defer func() {
			listLock.Unlock()
			dispatchKeyFrame()
		}()

		log.Println("peers: ", peerConnections)
		for i, pc := range peerConnections {
			log.Println("signal for ", i, " peer")
			cs := pc.ConnectionState()
			log.Println("cs is ", cs)
			switch cs {
			case webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
				delete(peerConnections, i)
				log.Println("remove peer")
				return true
			}

			// map of sender we already are seanding, so we don't double send
			existingSenders := map[string]bool{}

			for _, sender := range pc.GetSenders() {
				if sender.Track() == nil {
					continue
				}

				existingSenders[sender.Track().ID()] = true

				// If we have a RTPSender that doesn't map to a existing track remove and signal
				if _, ok := trackLocals[sender.Track().ID()]; !ok {
					if err := pc.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Don't receive videos we are sending, make sure we don't have loopback
			for _, receiver := range pc.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				existingSenders[receiver.Track().ID()] = true
			}

			// Add all track we aren't sending yet to the PeerConnection
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					if _, err := pc.AddTrack(trackLocals[trackID]); err != nil {
						return true
					}
				}
			}

			offer, err := pc.CreateOffer(nil)
			if err != nil {
				log.Println(err)
				return true
			}

			// Цикл здесь для того, чтобы поймать нужный сигналинг стейт
			// для установки нового оффера
			pc.mux.Lock()
			if pc.answerPending {
				pc.mux.Unlock()
				log.Println("wait answer")
				return true
			}
			pc.mux.Unlock()

			if err = pc.SetLocalDescription(offer); err != nil {
				log.Println(err)
				return true
			}

			offerString, err := json.Marshal(offer)
			if err != nil {
				return true
			}

			if err = pc.websocket.WriteJSON(&websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}

			pc.mux.Lock()
			pc.answerPending = true
			pc.mux.Unlock()
			log.Println("pc.answerPending = true")
		}

		return
	}

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			log.Println("max attempts reached: ", syncAttempt)
			return
		}

		if !attemptSync() {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}
}

// dispatchKeyFrame sends a keyframe to all PeerConnections, used everytime a new user joins the call
func dispatchKeyFrame() {
	listLock.Lock()
	defer listLock.Unlock()

	for i := range peerConnections {
		for _, receiver := range peerConnections[i].GetReceivers() {
			if receiver.Track() == nil {
				continue
			}

			_ = peerConnections[i].WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}

// Обработчик входящих сообщений
func websocketHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP request to Websocket
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}

	c := &threadSafeWriter{unsafeConn, sync.Mutex{}}

	// When this frame returns close the Websocket
	defer c.Close() //nolint

	message := &websocketMessage{}

	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		} else if err := json.Unmarshal(raw, &message); err != nil {
			log.Println(err)
			return
		}

		log.Println()
		log.Println("received: ", message.Event)

		listLock.Lock()
		pc := peerConnections[message.Id]
		listLock.Unlock()

		switch message.Event {
		case "candidate":
			candidate := webrtc.ICECandidateInit{}
			if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
				log.Println(err)
				return
			}

			if err := pc.AddICECandidate(candidate); err != nil {
				log.Println(err)
				return
			}
		// Предварительная установка вебртс соединения
		case "offer":
			var err error
			peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				log.Print(err)
				return
			}
			// When this frame returns close the PeerConnection
			defer peerConnection.Close() //nolint

			// Accept one audio and one video track incoming
			for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
				if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
					Direction: webrtc.RTPTransceiverDirectionRecvonly,
				}); err != nil {
					log.Print(err)
					return
				}
			}

			pc = &peerConnectionState{peerConnection, c, sync.Mutex{}, false, false}
			// Add our new PeerConnection to global list
			listLock.Lock()
			peerConnections[message.Id] = pc
			listLock.Unlock()

			// If PeerConnection is closed remove it from global list
			pc.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
				log.Println("connection state: ", p)
				switch p {
				case webrtc.PeerConnectionStateFailed:
					if err := pc.Close(); err != nil {
						log.Print(err)
					}
				case webrtc.PeerConnectionStateClosed:
					signalPeerConnections("peer closed")
				}
			})

			pc.OnICEConnectionStateChange(func(is webrtc.ICEConnectionState) {
				log.Println("ice connection state: ", is)
			})

			pc.OnSignalingStateChange(func(ss webrtc.SignalingState) {
				log.Println("ss is ", ss)
			})

			pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
				// Create a track to fan out our incoming video to all peers
				trackLocal := addTrack(t)
				defer removeTrack(trackLocal)

				buf := make([]byte, 1500)
				for {
					i, _, err := t.Read(buf)
					if err != nil {
						return
					}

					if _, err = trackLocal.Write(buf[:i]); err != nil {
						return
					}
				}
			})

			//Устанавливаем оффер с клиент и отправляем обратно ансвер
			description := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &description); err != nil {
				log.Println(err)
				return
			}

			if err := pc.SetRemoteDescription(description); err != nil {
				log.Println(err)
				return
			}

			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Println(err)
				return
			}

			if err = pc.SetLocalDescription(answer); err != nil {
				log.Println(err)
				return
			}

			// Отправляем локал дескрипшен, потому что в ансвере нет айс-кандидатов
			answerString, err := json.Marshal(pc.LocalDescription())
			if err != nil {
				return
			}

			if err = c.WriteJSON(&websocketMessage{
				Event: "answer",
				Data:  string(answerString),
			}); err != nil {
				return
			}

			// Signal for the new PeerConnection
			signalPeerConnections("init")

		// получаем оффер с новыми треками
		case "publish":
			description := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &description); err != nil {
				log.Println(err)
				return
			}

			if err := pc.SetRemoteDescription(description); err != nil {
				log.Println(err)
				return
			}

			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Println(err)
				return
			}

			if err = pc.SetLocalDescription(answer); err != nil {
				log.Println(err)
				return
			}

			answerString, err := json.Marshal(answer)
			if err != nil {
				return
			}

			if err = c.WriteJSON(&websocketMessage{
				Event: "answer",
				Data:  string(answerString),
			}); err != nil {
				return
			}

		// получаем ответ что получили новые треки
		case "answer":
			pc.mux.Lock()
			description := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &description); err != nil {
				log.Println(err)
				return
			}

			if err := pc.SetRemoteDescription(description); err != nil {
				log.Println(err)
				return
			}

			pc.answerPending = false
			log.Println("pc.answerPending = false")
			pc.mux.Unlock()
		}
	}
}

// Helper to make Gorilla Websockets threadsafe
type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

func (t *threadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}
