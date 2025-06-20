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
	"google.golang.org/protobuf/types/known/durationpb"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	sampleRate              = 8000
	reinitializationTimeout = 4*time.Minute + 50*time.Second
)

// GoogleResult holds transcription and streaming state info.
type GoogleResult struct {
	Result            *speechpb.StreamingRecognizeResponse
	Error             error
	TotalBilledTime   time.Duration
	Reinitialized     bool
	ReinitializedInfo string
}

// GoogleService streams audio to Google Speech-to-Text v2 API.
type GoogleService struct {
	languageCode   string
	privateKeyPath string
	enhancedMode   bool
	domainModel    string
	speechContext  []string
	client         speech.StreamingRecognizeClient

	sync.RWMutex
}

// NewGoogleService creates a new GoogleService using Speech-to-Text v2.
func NewGoogleService(privateKeyPath string, languageCode string, speechContext []string) (*GoogleService, error) {
	if len(strings.TrimSpace(privateKeyPath)) == 0 {
		return nil, errors.New("private key path is empty")
	}

	err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to set Google credential's env: %v", err)
	}

	g := GoogleService{
		languageCode:   languageCode,
		privateKeyPath: privateKeyPath,
		domainModel:    "phone_call",
		enhancedMode:   false,
		speechContext:  speechContext,
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

	ctx := context.Background()
	client, err := speech.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	stream, err := client.StreamingRecognize(ctx)
	if err != nil {
		return nil, err
	}

	g.client = stream

	sc := &speechpb.SpeechContext{Phrases: speechContext}

	if err := g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					Encoding:                   speechpb.RecognitionConfig_LINEAR16,
					SampleRateHertz:            sampleRate,
					LanguageCode:               g.languageCode,
					Model:                     g.domainModel,
					UseEnhanced:               g.enhancedMode,
					SpeechContexts:            []*speechpb.SpeechContext{sc},
					EnableAutomaticPunctuation: true,
					EnableWordTimeOffsets:      true,
					EnableSpokenPunctuation:    wrapperspb.Bool(true),
				},
				InterimResults:          true,
				SingleUtterance:         false,
				EnableVoiceActivityEvents: true,
				VoiceActivityTimeout: &speechpb.StreamingRecognitionConfig_VoiceActivityTimeout{
					SpeechStartTimeout: durationpb.New(30 * time.Second),
					SpeechEndTimeout:   durationpb.New(1 * time.Second),
				},
			},
		},
	}); err != nil {
		return nil, err
	}

	return &g, nil
}

// StartStreaming sends audio chunks from the channel to Google.
func (g *GoogleService) StartStreaming(ctx context.Context, stream <-chan []byte) <-chan error {
	errCh := make(chan error)
	go func() {
		defer close(errCh)
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-stream:
				g.RLock()
				err := g.client.Send(&speechpb.StreamingRecognizeRequest{
					StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
						AudioContent: s,
					},
				})
				g.RUnlock()
				if err != nil {
					errCh <- fmt.Errorf("streaming error: %v", err)
					return
				}
			}
		}
	}()
	return errCh
}

// SpeechToTextResponse reads streaming responses from Google, handles reinitialization events.
func (g *GoogleService) SpeechToTextResponse(ctx context.Context) <-chan GoogleResult {
	resultCh := make(chan GoogleResult)
	timer := time.NewTimer(reinitializationTimeout)

	go func() {
		defer close(resultCh)

		for {
			select {
			case <-ctx.Done():
				return

			case <-timer.C:
				g.Lock()
				resultCh <- GoogleResult{
					Reinitialized:     true,
					ReinitializedInfo: fmt.Sprintf("reinitialized client after %v", reinitializationTimeout),
				}
				g.Unlock()
				timer.Reset(reinitializationTimeout)
				return

			default:
				g.RLock()
				resp, err := g.client.Recv()
				g.RUnlock()

				if err == io.EOF {
					resultCh <- GoogleResult{Error: io.EOF}
					return
				}

				if err != nil {
					// Detect stream canceled by Google (e.g. silence timeout)
					if status.Code(err) == codes.Canceled {
						resultCh <- GoogleResult{
							Error:             err,
							Reinitialized:     true,
							ReinitializedInfo: "stream canceled by Google (possible silence or timeout)",
						}
						return
					}
					resultCh <- GoogleResult{Error: err}
					return
				}

				// Detect server-side reinitialization event (only on v2 API)
				if rr := resp.GetReinitializeResponse(); rr != nil {
					resultCh <- GoogleResult{
						Reinitialized:     true,
						ReinitializedInfo: rr.GetInfo(),
					}
					continue
				}

				resultCh <- GoogleResult{Result: resp}
			}
		}
	}()
	return resultCh
}

// Close cleanly closes the streaming client.
func (g *GoogleService) Close() error {
	g.Lock()
	defer g.Unlock()
	return g.client.CloseSend()
}

// ReinitializeClient closes old stream and creates a new streaming client and sends initial config.
func (g *GoogleService) ReinitializeClient() error {
	ctx := context.Background()

	client, err := speech.NewClient(ctx)
	if err != nil {
		return err
	}

	stream, err := client.StreamingRecognize(ctx)
	if err != nil {
		return err
	}

	g.Lock()
	g.client = stream
	g.Unlock()

	sc := &speechpb.SpeechContext{Phrases: g.speechContext}

	return g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					Encoding:                   speechpb.RecognitionConfig_LINEAR16,
					SampleRateHertz:            sampleRate,
					LanguageCode:               g.languageCode,
					Model:                     g.domainModel,
					UseEnhanced:               g.enhancedMode,
					SpeechContexts:            []*speechpb.SpeechContext{sc},					
					EnableAutomaticPunctuation: true,
					EnableWordTimeOffsets:      true,
					EnableSpokenPunctuation:    wrapperspb.Bool(true),
				},
				InterimResults:          true,
				SingleUtterance:         false,
				EnableVoiceActivityEvents: true,
				VoiceActivityTimeout: &speechpb.StreamingRecognitionConfig_VoiceActivityTimeout{
					SpeechStartTimeout: durationpb.New(30 * time.Second),
					SpeechEndTimeout:   durationpb.New(1 * time.Second),
				},
			},
		},
	})
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
