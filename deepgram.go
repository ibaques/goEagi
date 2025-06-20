package goEagi

import (
	"context"
	"encoding/json"
	"fmt"	
	"time"

	"github.com/gorilla/websocket"
)

type DeepgramResult struct {
	Timestamp time.Time
	Text      string
	IsFinal   bool
	Error     error
}

type DeepgramService struct {
	conn         *websocket.Conn
	url          string
	resultStream chan DeepgramResult
	lang         string
	apiKey       string
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewDeepgramService(ctx context.Context, apiKey string, lang string) (*DeepgramService, error) {
	ctx, cancel := context.WithCancel(ctx)

	url := fmt.Sprintf("wss://api.deepgram.com/v1/listen?language=%s&encoding=linear16&sample_rate=8000", lang)

	header := map[string][]string{
		"Authorization": {fmt.Sprintf("Token %s", apiKey)},
	}

	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to connect to Deepgram: %w", err)
	}

	s := &DeepgramService{
		conn:         conn,
		url:          url,
		resultStream: make(chan DeepgramResult, 100),
		lang:         lang,
		apiKey:       apiKey,
		ctx:          ctx,
		cancel:       cancel,
	}

	go s.listenResponses()

	return s, nil
}

// Stream sends audio bytes to Deepgram.
func (s *DeepgramService) Stream(data []byte) error {
	s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

// Close terminates the session.
func (s *DeepgramService) Close() error {
	s.cancel()
	return s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

// Results returns a channel with Deepgram transcription results.
func (s *DeepgramService) Results() <-chan DeepgramResult {
	return s.resultStream
}

// listenResponses parses messages from Deepgram.
func (s *DeepgramService) listenResponses() {
	defer close(s.resultStream)

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			_, msg, err := s.conn.ReadMessage()
			if err != nil {
				s.resultStream <- DeepgramResult{Error: fmt.Errorf("read error: %w", err)}
				return
			}

			var raw map[string]interface{}
			if err := json.Unmarshal(msg, &raw); err != nil {
				continue
			}

			channel, ok := raw["channel"].(map[string]interface{})
			if !ok {
				continue
			}

			alts, ok := channel["alternatives"].([]interface{})
			if !ok || len(alts) == 0 {
				continue
			}

			alt, ok := alts[0].(map[string]interface{})
			if !ok {
				continue
			}

			text, _ := alt["transcript"].(string)
			isFinal := raw["is_final"] == true

			s.resultStream <- DeepgramResult{
				Timestamp: time.Now(),
				Text:      text,
				IsFinal:   isFinal,
			}
		}
	}
}
