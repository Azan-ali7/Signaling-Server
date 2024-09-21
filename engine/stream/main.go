package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// Define constants and variables
const (
	webPort        = ":8080"
	deepgramURL    = "wss://api.deepgram.com/v1/listen"
	deepgramAPIKey = "5e270a358989702f8069eee0167a2a802a6348bc"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)

	fmt.Printf("Starting server at http://localhost%s\n", webPort)
	log.Fatal(http.ListenAndServe(webPort, nil))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket Upgrade Error:", err)
		return
	}
	defer conn.Close()

	peerConnection, err := createPeerConnection()
	if err != nil {
		log.Println("Failed to create PeerConnection:", err)
		return
	}

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		if err := conn.WriteJSON(map[string]interface{}{
			"type":      "candidate",
			"candidate": candidate.ToJSON(),
		}); err != nil {
			log.Println("Failed to send ICE candidate:", err)
		}
	})

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("WebSocket Read Error:", err)
			break
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Println("JSON Unmarshal Error:", err)
			continue
		}

		switch msg["type"] {
		case "offer":
			offer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  msg["sdp"].(string),
			}

			if err := peerConnection.SetRemoteDescription(offer); err != nil {
				log.Println("Failed to set remote description:", err)
				return
			}

			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				log.Println("Failed to create answer:", err)
				return
			}
			if err := peerConnection.SetLocalDescription(answer); err != nil {
				log.Println("Failed to set local description:", err)
				return
			}

			if err := conn.WriteJSON(map[string]interface{}{
				"type": "answer",
				"sdp":  answer.SDP,
			}); err != nil {
				log.Println("Failed to send answer:", err)
			}

		case "candidate":
			candidate := webrtc.ICECandidateInit{
				Candidate: msg["candidate"].(string),
			}
			if err := peerConnection.AddICECandidate(candidate); err != nil {
				log.Println("Failed to add ICE candidate:", err)
			}

		case "start-recording":
			log.Println("Starting recording")

		case "stop-recording":
			log.Println("Stopping recording")
			extractAudio()
			transcribeAudio()
		}
	}
}

func createPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	webmFile, err := os.Create("video.webm")
	if err != nil {
		return nil, err
	}

	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("New DataChannel %s %d\n", d.Label(), d.ID())

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if _, err := webmFile.Write(msg.Data); err != nil {
				log.Println("Error writing to WebM file:", err)
			}
		})
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateDisconnected {
			fmt.Println("Peer disconnected")
			if err := webmFile.Close(); err != nil {
				log.Println("Error closing WebM file:", err)
			}
			extractAudio()
			transcribeAudio()
			os.Exit(0)
		}
	})

	return peerConnection, nil
}

func extractAudio() {
	cmd := exec.Command("ffmpeg", "-i", "video.webm", "-vn", "-acodec", "libmp3lame", "-q:a", "4", "audio.mp3")
	err := cmd.Run()
	if err != nil {
		log.Printf("Error extracting audio: %v", err)
	} else {
		log.Println("Audio extracted successfully")
	}
}

func transcribeAudio() {
	audioFile, err := os.Open("small.mp3")
	if err != nil {
		log.Printf("Error opening audio file: %v", err)
		return
	}
	defer audioFile.Close()

	// Verify audio file size
	audioData, err := ioutil.ReadAll(audioFile)
	if err != nil {
		log.Printf("Error reading audio file: %v", err)
		return
	}
	if len(audioData) == 0 {
		log.Println("Error: Audio file is empty")
		return
	}
	log.Printf("Audio file size: %d bytes", len(audioData))

	deepgramConn, _, err := websocket.DefaultDialer.Dial(deepgramURL, http.Header{
		"Authorization": []string{"Token " + deepgramAPIKey},
	})
	if err != nil {
		log.Printf("Error connecting to Deepgram: %v", err)
		return
	}
	defer deepgramConn.Close()

	fmt.Println("Deepgram connection opened successfully")

	chunkSize := 100 * 1024 // Adjust this based on your needs (100 ms in bytes)
	for start := 0; start < len(audioData); start += chunkSize {
		end := start + chunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		err = deepgramConn.WriteMessage(websocket.BinaryMessage, audioData[start:end])
		if err != nil {
			log.Printf("Error sending audio data to Deepgram: %v", err)
			return
		}

		time.Sleep(100 * time.Millisecond) // Wait for 100 ms before sending the next chunk
	}

	for {
		_, message, err := deepgramConn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message from Deepgram: %v", err)
			fmt.Println("Deepgram connection closed or failed")
			return
		}

		var result map[string]interface{}
		err = json.Unmarshal(message, &result)
		if err != nil {
			log.Printf("Error parsing Deepgram response: %v", err)
			continue
		}

		channel, ok := result["channel"].(map[string]interface{})
		if !ok {
			continue
		}

		alternatives, ok := channel["alternatives"].([]interface{})
		if !ok || len(alternatives) == 0 {
			continue
		}

		alternative, ok := alternatives[0].(map[string]interface{})
		if !ok {
			continue
		}

		transcript, ok := alternative["transcript"].(string)
		if !ok {
			continue
		}

		if transcript == "" {
			log.Println("Warning: Received empty transcript from Deepgram")
		} else {
			fmt.Printf("Transcription: %s\n", transcript)
		}

		if isFinal, ok := result["is_final"].(bool); ok && isFinal {
			break
		}
	}

	fmt.Println("Deepgram connection closed successfully")
}

// func transcribeAudio() {
// 	audioData, err := ioutil.ReadFile("sample.mp3")
// 	if err != nil {
// 		log.Printf("Error reading audio file: %v", err)
// 		return
// 	}

// 	// Verify audio file size
// 	if len(audioData) == 0 {
// 		log.Println("Error: Audio file is empty")
// 		return
// 	}
// 	log.Printf("Audio file size: %d bytes", len(audioData))

// 	deepgramConn, _, err := websocket.DefaultDialer.Dial(deepgramURL, http.Header{
// 		"Authorization": []string{"Token " + deepgramAPIKey},
// 	})
// 	if err != nil {
// 		log.Printf("Error connecting to Deepgram: %v", err)
// 		return
// 	}
// 	defer deepgramConn.Close()

// 	fmt.Println("Deepgram connection opened successfully")

// 	err = deepgramConn.WriteMessage(websocket.BinaryMessage, audioData)
// 	if err != nil {
// 		log.Printf("Error sending audio data to Deepgram: %v", err)
// 		return
// 	}

// 	for {
// 		_, message, err := deepgramConn.ReadMessage()
// 		if err != nil {
// 			log.Printf("Error reading message from Deepgram: %v", err)
// 			fmt.Println("Deepgram connection closed or failed")
// 			return
// 		}

// 		var result map[string]interface{}
// 		err = json.Unmarshal(message, &result)
// 		if err != nil {
// 			log.Printf("Error parsing Deepgram response: %v", err)
// 			continue
// 		}

// 		if channel, ok := result["channel"].(map[string]interface{}); ok {
// 			if alternatives, ok := channel["alternatives"].([]interface{}); ok && len(alternatives) > 0 {
// 				if alternative, ok := alternatives[0].(map[string]interface{}); ok {
// 					if transcript, ok := alternative["transcript"].(string); ok {
// 						if transcript == "" {
// 							log.Println("Warning: Received empty transcript from Deepgram")
// 						} else {
// 							fmt.Printf("Transcription: %s\n", transcript)
// 						}
// 					}
// 				}
// 			}
// 		}

// 		if isFinal, ok := result["is_final"].(bool); ok && isFinal {
// 			break
// 		}
// 	}

// 	fmt.Println("Deepgram connection closed successfully")
// }

// package main

// import (
// 	"encoding/json"
// 	"fmt"
// 	"io/ioutil"
// 	"log"
// 	"net/http"
// 	"os"
// 	"os/exec"
// 	"time"

// 	"github.com/gorilla/websocket"
// 	"github.com/pion/webrtc/v4"
// )

// const (
// 	webPort        = ":8080"
// 	deepgramURL    = "wss://api.deepgram.com/v1/listen"
// 	deepgramAPIKey = "5e270a358989702f8069eee0167a2a802a6348bc"
// )

// var upgrader = websocket.Upgrader{
// 	ReadBufferSize:  4096,
// 	WriteBufferSize: 4096,
// 	CheckOrigin: func(r *http.Request) bool {
// 		return true
// 	},
// }

// func main() {
// 	http.HandleFunc("/ws", handleWebSocket)
// 	fs := http.FileServer(http.Dir("./web"))
// 	http.Handle("/", fs)

// 	fmt.Printf("Starting server at http://localhost%s\n", webPort)
// 	log.Fatal(http.ListenAndServe(webPort, nil))
// }

// func handleWebSocket(w http.ResponseWriter, r *http.Request) {
// 	conn, err := upgrader.Upgrade(w, r, nil)
// 	if err != nil {
// 		log.Println("WebSocket Upgrade Error:", err)
// 		return
// 	}
// 	defer conn.Close()

// 	peerConnection, err := createPeerConnection()
// 	if err != nil {
// 		log.Println("Failed to create PeerConnection:", err)
// 		return
// 	}

// 	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
// 		if candidate == nil {
// 			return
// 		}

// 		if err := conn.WriteJSON(map[string]interface{}{
// 			"type":      "candidate",
// 			"candidate": candidate.ToJSON(),
// 		}); err != nil {
// 			log.Println("Failed to send ICE candidate:", err)
// 		}
// 	})

// 	for {
// 		_, message, err := conn.ReadMessage()
// 		if err != nil {
// 			log.Println("WebSocket Read Error:", err)
// 			break
// 		}

// 		var msg map[string]interface{}
// 		if err := json.Unmarshal(message, &msg); err != nil {
// 			log.Println("JSON Unmarshal Error:", err)
// 			continue
// 		}

// 		switch msg["type"] {
// 		case "offer":
// 			offer := webrtc.SessionDescription{
// 				Type: webrtc.SDPTypeOffer,
// 				SDP:  msg["sdp"].(string),
// 			}

// 			if err := peerConnection.SetRemoteDescription(offer); err != nil {
// 				log.Println("Failed to set remote description:", err)
// 				return
// 			}

// 			answer, err := peerConnection.CreateAnswer(nil)
// 			if err != nil {
// 				log.Println("Failed to create answer:", err)
// 				return
// 			}
// 			if err := peerConnection.SetLocalDescription(answer); err != nil {
// 				log.Println("Failed to set local description:", err)
// 				return
// 			}

// 			if err := conn.WriteJSON(map[string]interface{}{
// 				"type": "answer",
// 				"sdp":  answer.SDP,
// 			}); err != nil {
// 				log.Println("Failed to send answer:", err)
// 			}

// 		case "candidate":
// 			candidate := webrtc.ICECandidateInit{
// 				Candidate: msg["candidate"].(string),
// 			}
// 			if err := peerConnection.AddICECandidate(candidate); err != nil {
// 				log.Println("Failed to add ICE candidate:", err)
// 			}

// 		case "start-recording":
// 			log.Println("Starting recording")

// 		case "stop-recording":
// 			log.Println("Stopping recording")
// 			extractAudio()
// 			transcribeAudio()
// 		}
// 	}
// }

// func createPeerConnection() (*webrtc.PeerConnection, error) {
// 	config := webrtc.Configuration{
// 		ICEServers: []webrtc.ICEServer{
// 			{
// 				URLs: []string{"stun:stun.l.google.com:19302"},
// 			},
// 		},
// 	}

// 	peerConnection, err := webrtc.NewPeerConnection(config)
// 	if err != nil {
// 		return nil, err
// 	}

// 	webmFile, err := os.Create("video.webm")
// 	if err != nil {
// 		return nil, err
// 	}

// 	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
// 		fmt.Printf("New DataChannel %s %d\n", d.Label(), d.ID())

// 		d.OnMessage(func(msg webrtc.DataChannelMessage) {
// 			if _, err := webmFile.Write(msg.Data); err != nil {
// 				log.Println("Error writing to WebM file:", err)
// 			}
// 		})
// 	})

// 	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
// 		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())

// 		if connectionState == webrtc.ICEConnectionStateDisconnected {
// 			fmt.Println("Peer disconnected")
// 			if err := webmFile.Close(); err != nil {
// 				log.Println("Error closing WebM file:", err)
// 			}
// 			extractAudio()
// 			transcribeAudio()
// 			os.Exit(0)
// 		}
// 	})

// 	return peerConnection, nil
// }

// func extractAudio() {
// 	cmd := exec.Command("ffmpeg", "-i", "video.webm", "-vn", "-acodec", "libmp3lame", "-q:a", "4", "audio.mp3")
// 	err := cmd.Run()
// 	if err != nil {
// 		log.Printf("Error extracting audio: %v", err)
// 	} else {
// 		log.Println("Audio extracted successfully")
// 	}
// }

// func transcribeAudio() {
// 	audioFile, err := os.Open("audio.mp3")
// 	if err != nil {
// 		log.Printf("Error opening audio file: %v", err)
// 		return
// 	}
// 	defer audioFile.Close()

// 	audioData, err := ioutil.ReadAll(audioFile)
// 	if err != nil {
// 		log.Printf("Error reading audio file: %v", err)
// 		return
// 	}
// 	if len(audioData) == 0 {
// 		log.Println("Error: Audio file is empty")
// 		return
// 	}
// 	log.Printf("Audio file size: %d bytes", len(audioData))

// 	deepgramConn, _, err := websocket.DefaultDialer.Dial(deepgramURL, http.Header{
// 		"Authorization": []string{"Token " + deepgramAPIKey},
// 	})
// 	if err != nil {
// 		log.Printf("Error connecting to Deepgram: %v", err)
// 		return
// 	}
// 	defer deepgramConn.Close()
// 	fmt.Println("Deepgram connection opened successfully")

// 	chunkSize := 100 * 1024 // 100 KB per chunk
// 	transcript := ""        // To accumulate the entire transcript

// 	for start := 0; start < len(audioData); start += chunkSize {
// 		end := start + chunkSize
// 		if end > len(audioData) {
// 			end = len(audioData)
// 		}

// 		err = deepgramConn.WriteMessage(websocket.BinaryMessage, audioData[start:end])
// 		if err != nil {
// 			log.Printf("Error sending audio data to Deepgram: %v", err)
// 			return
// 		}

// 		time.Sleep(100 * time.Millisecond) // Delay between sending chunks
// 	}

// 	// Reading messages from Deepgram
// 	for {
// 		_, message, err := deepgramConn.ReadMessage()
// 		if err != nil {
// 			log.Printf("Error reading message from Deepgram: %v", err)
// 			break
// 		}

// 		var result map[string]interface{}
// 		if err := json.Unmarshal(message, &result); err != nil {
// 			log.Printf("Error parsing Deepgram response: %v", err)
// 			continue
// 		}

// 		if channel, ok := result["channel"].(map[string]interface{}); ok {
// 			if alternatives, ok := channel["alternatives"].([]interface{}); ok && len(alternatives) > 0 {
// 				if alternative, ok := alternatives[0].(map[string]interface{}); ok {
// 					if partialTranscript, ok := alternative["transcript"].(string); ok {
// 						transcript += partialTranscript
// 						fmt.Printf("Partial Transcript: %s\n", partialTranscript)
// 					}
// 				}
// 			}
// 		}

// 		if isFinal, ok := result["is_final"].(bool); ok && isFinal {
// 			break
// 		}
// 	}

// 	if transcript == "" {
// 		fmt.Println("Warning: No transcript received")
// 	} else {
// 		fmt.Printf("Complete Transcript: %s\n", transcript)
// 	}
// 	fmt.Println("Deepgram connection closed successfully")
// }
