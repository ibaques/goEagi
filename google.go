package goEagi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	speechpb "cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	sampleRate             = 8000
	reinitializationTimeout = 4*time.Minute + 50*time.Second
)

type GoogleResult struct {
	Result            *speechpb.StreamingRecognizeResponse
	Error             error
	TotalBilledTime   time.Duration
	Reinitialized     bool
	ReinitializedInfo string
}

type GoogleService struct {
	languageCode   string
	privateKeyPath string
	enhancedMode   bool
	domainModel    string
	speechContext  []string
	client         speech.Speech_StreamingRecognizeClient

	sync.RWMutex
}

func NewGoogleService(privateKeyPath string, languageCode string, speechContext []string) (*GoogleService, error) {
	if strings.TrimSpace(privateKeyPath) == "" {
		return nil, errors.New("private key path is empty")
	}
	if err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", privateKeyPath); err != nil {
		return nil, fmt.Errorf("failed to set GOOGLE_APPLICATION_CREDENTIALS: %v", err)
	}

	ctx := context.Background()
	client, err := speech.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	streamingClient, err := client.StreamingRecognize(ctx)
	if err != nil {
		return nil, err
	}

	g := &GoogleService{
		languageCode:   languageCode,
		privateKeyPath: privateKeyPath,
		domainModel:    "phone_call",
		enhancedMode:   false,
		speechContext:  speechContext,
		client:        streamingClient,
	}

	for _, v := range supportedEnhancedMode() {
		if v == languageCode {
			g.enhancedMode = true
			break
		}
	}
	for _, v := range supportedTelephony() {
		if v == languageCode {
			g.domainModel = "telephony"
			break
		}
	}
	for _, v := range supportedDefault() {
		if v == languageCode {
			g.domainModel = "default"
			break
		}
	}

	phraseSet := &speechpb.PhraseSet{
		Phrases: make([]*speechpb.Phrase, len(speechContext)),
	}
	for i, phrase := range speechContext {
		phraseSet.Phrases[i] = &speechpb.Phrase{Value: phrase}
	}

	err = g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					Encoding:        speechpb.AudioEncoding_LINEAR16,
					SampleRateHertz: sampleRate,
					LanguageCode:    g.languageCode,
					Model:           g.domainModel,
					UseEnhanced:     g.enhancedMode,
					EnableAutomaticPunctuation: true,
					EnableWordTimeOffsets:      true,
					EnableSpokenPunctuation:    wrapperspb.Bool(true),
					SpeechAdaptation: &speechpb.SpeechAdaptation{
						PhraseSets: []*speechpb.PhraseSet{phraseSet},
					},
				},
				InterimResults:            true,
				SingleUtterance:           false,
				EnableVoiceActivityEvents: true,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	return g, nil
}

func (g *GoogleService) StartStreaming(ctx context.Context, stream <-chan []byte) <-chan error {
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-stream:
				if !ok {
					return
				}
				g.RLock()
				err := g.client.Send(&speechpb.StreamingRecognizeRequest{
					StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
						AudioContent: data,
					},
				})
				g.RUnlock()
				if err != nil {
					errCh <- fmt.Errorf("streaming send error: %w", err)
					return
				}
			}
		}
	}()
	return errCh
}

func (g *GoogleService) SpeechToTextResponse(ctx context.Context) <-chan GoogleResult {
	results := make(chan GoogleResult)
	go func() {
		defer close(results)
		timer := time.NewTimer(reinitializationTimeout)
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				results <- GoogleResult{
					Reinitialized:     true,
					ReinitializedInfo: fmt.Sprintf("reinitialized client after %v", reinitializationTimeout),
				}
				timer.Reset(reinitializationTimeout)
				return
			default:
				g.RLock()
				resp, err := g.client.Recv()
				g.RUnlock()

				if err == io.EOF {
					results <- GoogleResult{Error: io.EOF}
					return
				}
				if err != nil {
					results <- GoogleResult{Error: err}
					return
				}

				results <- GoogleResult{Result: resp}
			}
		}
	}()
	return results
}

func (g *GoogleService) Close() error {
	g.Lock()
	defer g.Unlock()
	return g.client.CloseSend()
}

func supportedEnhancedMode() []string {
	return []string{"es-US", "en-GB", "en-US", "fr-FR", "ja-JP", "pt-BR", "ru-RU", "es-ES"}
}
func supportedTelephony() []string {
	return []string{"pt-PT", "nl-NL"}
}
func supportedDefault() []string {
	return []string{"ca-ES", "da-DK"}
}
