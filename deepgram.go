package goEagi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"
)

// DeepgramResult contiene un resultado parcial de transcripción de Deepgram.
type DeepgramResult struct {
	Timestamp float64 // timestamp en segundos dentro del audio
	Text      string  // texto transcrito parcial
	IsFinal   bool    // si es resultado final
}

// DeepgramService mantiene la conexión websocket con Deepgram.
type DeepgramService struct {
	apiKey       string
	language     string
	sampleRate   int
	encoding     string

	conn         *websocket.Conn
	writeMutex   sync.Mutex // para evitar escrituras concurrentes
	resultChan   chan DeepgramResult
	doneChan     chan struct{}

	connected    bool
	connectMutex sync.Mutex
}

// NewDeepgramService crea una nueva instancia y conecta al websocket Deepgram.
// language: código BCP-47 (ej: "es-ES")
// sampleRate: tasa de muestreo (ej: 8000)
// encoding: tipo de codificación (ej: "linear16")
func NewDeepgramService(apiKey, language string, sampleRate int, encoding string) (*DeepgramService, error) {
	dg := &DeepgramService{
		apiKey:     apiKey,
		language:   language,
		sampleRate: sampleRate,
		encoding:   encoding,
		resultChan: make(chan DeepgramResult, 100),
		doneChan:   make(chan struct{}),
	}

	err := dg.connectWebSocket()
	if err != nil {
		return nil, fmt.Errorf("failed to connect Deepgram websocket: %w", err)
	}

	go dg.readLoop()

	return dg, nil
}

// connectWebSocket abre la conexión websocket al endpoint Deepgram.
func (d *DeepgramService) connectWebSocket() error {
	d.connectMutex.Lock()
	defer d.connectMutex.Unlock()

	u := url.URL{
		Scheme: "wss",
		Host:   "api.deepgram.com",
		Path:   "/v1/listen",
		RawQuery: fmt.Sprintf(
			"language=%s&encoding=%s&sample_rate=%d&punctuate=true&interim_results=true",
			url.QueryEscape(d.language),
			url.QueryEscape(d.encoding),
			d.sampleRate,
		),
	}

	header := http.Header{}
	header.Add("Authorization", "Token "+d.apiKey)

	log.Printf("[Deepgram] Connecting to %s", u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		return fmt.Errorf("Deepgram websocket dial error: %w", err)
	}

	d.conn = conn
	d.connected = true
	log.Printf("[Deepgram] Websocket connected")
	return nil
}

// StartStreaming envía el audio al websocket.
// Debe ser llamado en goroutine separada.
func (d *DeepgramService) StartStreaming(ctx context.Context, audioStream <-chan []byte) error {
	if d == nil {
		return fmt.Errorf("DeepgramService is nil")
	}

	d.connectMutex.Lock()
	if !d.connected || d.conn == nil {
		d.connectMutex.Unlock()
		return fmt.Errorf("Deepgram websocket is not connected")
	}
	d.connectMutex.Unlock()

	for {
		select {
		case <-ctx.Done():
			d.Close()
			return ctx.Err()

		case audio, ok := <-audioStream:
			if !ok {
				// canal cerrado, enviamos cierre websocket
				d.Close()
				return nil
			}

			d.writeMutex.Lock()
			err := d.conn.WriteMessage(websocket.BinaryMessage, audio)
			d.writeMutex.Unlock()
			if err != nil {
				d.Close()
				return fmt.Errorf("Deepgram write error: %w", err)
			}
		}
	}
}

// readLoop lee mensajes de texto JSON desde Deepgram y los parsea.
func (d *DeepgramService) readLoop() {
	defer close(d.resultChan)

	if d == nil {
		log.Printf("[Deepgram] readLoop: DeepgramService is nil")
		return
	}

	for {
		d.connectMutex.Lock()
		if !d.connected || d.conn == nil {
			d.connectMutex.Unlock()
			log.Printf("[Deepgram] readLoop: websocket disconnected")
			return
		}
		d.connectMutex.Unlock()

		_, message, err := d.conn.ReadMessage()
		if err != nil {
			log.Printf("[Deepgram] read error: %v", err)
			d.connectMutex.Lock()
			d.connected = false
			d.conn = nil
			d.connectMutex.Unlock()
			return
		}

		var resp deepgramResponse
		if err := json.Unmarshal(message, &resp); err != nil {
			log.Printf("[Deepgram] json unmarshal error: %v", err)
			continue
		}

		for _, channel := range resp.Channels {
			for _, alt := range channel.Alternatives {
				if alt.Transcript == "" {
					continue
				}
				// Emitir solo una vez por alternativa
				result := DeepgramResult{
					Text:      alt.Transcript,
					IsFinal:   alt.IsFinal,
				}
				if len(alt.Words) > 0 {
					result.Timestamp = alt.Words[0].Start
				}

				select {
				case d.resultChan <- result:
				default:
					// canal lleno, se descarta para evitar bloqueo
				}
				break
			}
		}
	}
}

// Results retorna el canal donde se reciben DeepgramResult.
func (d *DeepgramService) Results() <-chan DeepgramResult {
	return d.resultChan
}

// Close cierra la conexión websocket.
func (d *DeepgramService) Close() error {
	d.connectMutex.Lock()
	defer d.connectMutex.Unlock()

	if d.conn != nil {
		err := d.conn.Close()
		d.conn = nil
		d.connected = false
		return err
	}
	return nil
}

// Estructuras internas para parsear JSON Deepgram

type deepgramResponse struct {
	Channels []channel `json:"channels"`
}

type channel struct {
	Alternatives []alternative `json:"alternatives"`
}

type alternative struct {
	Transcript string  `json:"transcript"`
	IsFinal    bool    `json:"is_final"`
	Words      []word  `json:"words"`
}

type word struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Word  string  `json:"word"`
}
