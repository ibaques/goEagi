// Package goEagi of google.go provides a simplified interface
// for calling Google's speech to text service.

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
	"cloud.google.com/go/speech/apiv2/speechpb"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	sampleRate  = 8000
	domainModel = "telephony"

	reinitializationTimeout = 4*time.Minute + 50*time.Second
)

// GoogleResult is a struct that contains transcription result from Google Speech to Text service.
type GoogleResult struct {
	Result            *speechpb.StreamingRecognizeResponse
	Error             error
	Reinitialized     bool
	ReinitializedInfo string
}

// GoogleService is used to stream audio data to Google Speech to Text service.
type GoogleService struct {
	languageCode   string
	privateKeyPath string
	enhancedMode   bool
	speechContext  []string
	client         speechpb.Speech_StreamingRecognizeClient

	sync.RWMutex
}

// NewGoogleService creates a new GoogleService instance,
// it takes a privateKeyPath and set it in environment with key GOOGLE_APPLICATION_CREDENTIALS,
// a languageCode, example ["en-GB", "en-US", "ch", ...], see (https://cloud.google.com/speech-to-text/docs/languages),
// and a speech context, see (https://cloud.google.com/speech-to-text/docs/speech-adaptation).
func NewGoogleService(privateKeyPath string, languageCode string, speechContext []string) (*GoogleService, error) {
	if len(strings.TrimSpace(privateKeyPath)) == 0 {
		return nil, errors.New("private key path is empty")
	}

	err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to set Google credential's env: %v\n", err)
	}

	g := GoogleService{
		languageCode:   languageCode,
		privateKeyPath: privateKeyPath,
		enhancedMode:   false,
		speechContext:  speechContext,
	}

	for _, v := range supportedEnhancedMode() {
		if v == languageCode {
			g.enhancedMode = true
			break
		}
	}

	ctx := context.Background()

	client, err := speech.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	g.client, err = client.StreamingRecognize(ctx)
	if err != nil {
		return nil, err
	}

	sc := &speechpb.SpeechContext{Phrases: speechContext}

	diarizationConfig := &speechpb.SpeakerDiarizationConfig{
                EnableSpeakerDiarization: true,
                MinSpeakerCount:          2,
                MaxSpeakerCount:          2,
        }

	if err := g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{					
					DecodingConfig: &speechpb.RecognitionConfig_ExplicitDecodingConfig{
						ExplicitDecodingConfig: &speechpb.ExplicitDecodingConfig{
							Encoding:          speechpb.ExplicitDecodingConfig_LINEAR16,
							SampleRateHertz:   sampleRate,
							AudioChannelCount: 1,
						},
					},					
					Model:           domainModel,
					UseEnhanced:     g.enhancedMode,
					SpeechContexts:  []*speechpb.SpeechContext{sc},
					DiarizationConfig: diarizationConfig,
					EnableAutomaticPunctuation: true,
					EnableWordTimeOffsets: true,
					EnableSpokenPunctuation: wrapperspb.Bool(true),
				},
				InterimResults: true,
				SingleUtterance: false,
				EnableVoiceActivityEvents: true,
			},
		},
	}); err != nil {
		return nil, err
	}

	return &g, nil
}

// StartStreaming takes a reading channel of audio stream and sends it
// as a gRPC request to Google service through the initialized client.
func (g *GoogleService) StartStreaming(ctx context.Context, stream <-chan []byte) <-chan error {
	startStream := make(chan error)

	go func() {
		defer close(startStream)

		for {
			select {
			case <-ctx.Done():
				return

			case s := <-stream:
				g.RLock()
				if err := g.client.Send(&speechpb.StreamingRecognizeRequest{
					StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
						AudioContent: s,
					},
				}); err != nil {
					startStream <- fmt.Errorf("streaming error: %v\n", err)
					return
				}
				g.RUnlock()
			}
		}
	}()

	return startStream
}

// SpeechToTextResponse sends the transcription response from Google's SpeechToText.
func (g *GoogleService) SpeechToTextResponse(ctx context.Context) <-chan GoogleResult {
	googleResultStream := make(chan GoogleResult)

	go func() {
		defer close(googleResultStream)

		// reinitialize the client after a certain period of time, because Google's client has a timeout/quota limit of 5 min.
		timer := time.NewTimer(reinitializationTimeout)

		for {
			select {
			case <-ctx.Done():
				return

			case <-timer.C:
				g.Lock()
				googleResultStream <- GoogleResult{
					Reinitialized:     true,
					ReinitializedInfo: fmt.Sprintf("reinitialized client after %v", reinitializationTimeout),
				}

				if err := g.ReinitializeClient(); err != nil {
					googleResultStream <- GoogleResult{Error: fmt.Errorf("failed to reinitialize client: %v", err)}
					g.Unlock()
					return
				}

				googleResultStream <- GoogleResult{
					Reinitialized:     false,
					ReinitializedInfo: "reinitialized client successfully",
				}
				g.Unlock()

				timer.Reset(reinitializationTimeout)

			default:
				g.RLock()
				resp, err := g.client.Recv()
				g.RUnlock()

				if err == io.EOF {
					googleResultStream <- GoogleResult{Error: io.EOF}
					return
				}

				if err != nil {
					googleResultStream <- GoogleResult{Error: fmt.Errorf("cannot stream results: %v", err)}
					return
				}

				googleResultStream <- GoogleResult{Result: resp}			
			}
		}
	}()

	return googleResultStream
}

// Close closes the GoogleService.
func (g *GoogleService) Close() error {
	g.Lock()
	defer g.Unlock()
	return g.client.CloseSend()
}

// ReinitializeClient reinitializes the Google client.
func (g *GoogleService) ReinitializeClient() error {
	ctx := context.Background()

	client, err := speech.NewClient(ctx)
	if err != nil {
		return err
	}

	g.client, err = client.StreamingRecognize(ctx)
	if err != nil {
		return err
	}

	sc := &speechpb.SpeechContext{Phrases: g.speechContext}
	
	diarizationConfig := &speechpb.SpeakerDiarizationConfig{
                EnableSpeakerDiarization: true,
                MinSpeakerCount:          2,
                MaxSpeakerCount:          2,
        }
	
	if err := g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
                                Config: &speechpb.RecognitionConfig{
 					DecodingConfig: &speechpb.RecognitionConfig_ExplicitDecodingConfig{
						ExplicitDecodingConfig: &speechpb.ExplicitDecodingConfig{
							Encoding:          speechpb.ExplicitDecodingConfig_LINEAR16,
							SampleRateHertz:   sampleRate,
							AudioChannelCount: 1,
						},
					},
                                        LanguageCode:    g.languageCode,
                                        Model:           domainModel,
                                        UseEnhanced:     g.enhancedMode,
                                        SpeechContexts:  []*speechpb.SpeechContext{sc},
                                        DiarizationConfig: diarizationConfig,
                                        EnableAutomaticPunctuation: true,
					EnableWordTimeOffsets: true,
					EnableSpokenPunctuation: wrapperspb.Bool(true),
                                },
                                InterimResults: true,
                                SingleUtterance: false,
				EnableVoiceActivityEvents: true,
			},
		},
	}); err != nil {
		return err
	}

	return nil
}

// supportedEnhancedMode returns a list of supported language code for enhanced mode.
func supportedEnhancedMode() []string {
	return []string{"es-US", "en-GB", "en-US", "fr-FR", "ja-JP", "pt-BR", "ru-RU", "es-ES"}
}
