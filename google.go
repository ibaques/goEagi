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
	durationpb "google.golang.org/protobuf/types/known/durationpb"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	sampleRate              = 8000
	reinitializationTimeout = 4*time.Minute + 50*time.Second
)

// GoogleResult contains transcription results from Google Speech-to-Text service.
type GoogleResult struct {
	Result            *speechpb.StreamingRecognizeResponse
	Error             error
	TotalBilledTime   time.Duration
	Reinitialized     bool
	ReinitializedInfo string
}

// GoogleService streams audio data to Google Speech-to-Text.
type GoogleService struct {
	languageCode   string
	privateKeyPath string
	enhancedMode   bool
	domainModel    string
	speechContext  []string
	client         speech.Speech_StreamingRecognizeClient

	sync.RWMutex
}

// NewGoogleService creates a new GoogleService instance.
func NewGoogleService(privateKeyPath string, languageCode string, speechContext []string) (*GoogleService, error) {
	if len(strings.TrimSpace(privateKeyPath)) == 0 {
		return nil, errors.New("private key path is empty")
	}

	if err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", privateKeyPath); err != nil {
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

	streamingClient, err := client.StreamingRecognize(ctx)
	if err != nil {
		return nil, err
	}
	g.client = streamingClient

	// Adapt speechContext (phrases) into SpeechAdaptation with PhraseSet
	var phraseSet *speechpb.PhraseSet
	if len(speechContext) > 0 {
		phraseSet = &speechpb.PhraseSet{
			Phrases: make([]*speechpb.PhraseSet_Phrase, len(speechContext)),
		}
		for i, phrase := range speechContext {
			phraseSet.Phrases[i] = &speechpb.PhraseSet_Phrase{Value: phrase}
		}
	}

	// Build the StreamingRecognitionConfig
	config := &speechpb.RecognitionConfig{
		AudioEncoding:           speechpb.RecognitionConfig_LINEAR16,
		SampleRateHertz:         sampleRate,
		LanguageCode:            g.languageCode,
		EnableAutomaticPunctuation: true,
		EnableWordTimeOffsets:   true,
		EnableSpokenPunctuation: wrapperspb.Bool(true),
		// Adaptation: speech adaption with phrase sets
	}
	if phraseSet != nil {
		config.Adaptation = &speechpb.SpeechAdaptation{
			PhraseSets: []*speechpb.PhraseSet{phraseSet},
		}
	}

	// Optional diarization config — déjalo si lo necesitas, si no comenta estas líneas
	diarizationConfig := &speechpb.SpeakerDiarizationConfig{
		EnableSpeakerDiarization: false,
		MinSpeakerCount:          1,
		MaxSpeakerCount:          1,
	}
	config.SpeakerDiarizationConfig = diarizationConfig

	streamingConfig := &speechpb.StreamingRecognitionConfig{
		Config:                  config,
		InterimResults:          true,
		SingleUtterance:         false,
		EnableVoiceActivityEvents: true,
		VoiceActivityTimeout: &speechpb.StreamingRecognitionConfig_VoiceActivityTimeout{
			SpeechStartTimeout: durationpb.New(30 * time.Second),
			SpeechEndTimeout:   durationpb.New(500 * time.Millisecond),
		},
	}

	err = g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: streamingConfig,
		},
	})
	if err != nil {
		return nil, err
	}

	return &g, nil
}

// StartStreaming streams audio bytes to Google.
func (g *GoogleService) StartStreaming(ctx context.Context, stream <-chan []byte) <-chan error {
	startStream := make(chan error)

	go func() {
		defer close(startStream)

		for {
			select {
			case <-ctx.Done():
				return
			case s, ok := <-stream:
				if !ok {
					return
				}
				g.RLock()
				err := g.client.Send(&speechpb.StreamingRecognizeRequest{
					StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
						AudioContent: s,
					},
				})
				g.RUnlock()
				if err != nil {
					startStream <- fmt.Errorf("streaming error: %v", err)
					return
				}
			}
		}
	}()

	return startStream
}

// SpeechToTextResponse receives Google transcription responses.
func (g *GoogleService) SpeechToTextResponse(ctx context.Context) <-chan GoogleResult {
	googleResultStream := make(chan GoogleResult)

	go func() {
		defer close(googleResultStream)

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
				g.Unlock()
				timer.Reset(reinitializationTimeout)
				return

			default:
				g.RLock()
				resp, err := g.client.Recv()
				g.RUnlock()

				if err == io.EOF {
					googleResultStream <- GoogleResult{Error: io.EOF}
					return
				}

				if err != nil {
					googleResultStream <- GoogleResult{Error: err}
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

// ReinitializeClient recreates client and streaming client.
func (g *GoogleService) ReinitializeClient() error {
	ctx := context.Background()
	client, err := speech.NewClient(ctx)
	if err != nil {
		return err
	}

	streamingClient, err := client.StreamingRecognize(ctx)
	if err != nil {
		return err
	}

	g.client = streamingClient

	// Adapt speechContext (phrases) into SpeechAdaptation with PhraseSet
	var phraseSet *speechpb.PhraseSet
	if len(g.speechContext) > 0 {
		phraseSet = &speechpb.PhraseSet{
			Phrases: make([]*speechpb.PhraseSet_Phrase, len(g.speechContext)),
		}
		for i, phrase := range g.speechContext {
			phraseSet.Phrases[i] = &speechpb.PhraseSet_Phrase{Value: phrase}
		}
	}

	config := &speechpb.RecognitionConfig{
		AudioEncoding:           speechpb.RecognitionConfig_LINEAR16,
		SampleRateHertz:         sampleRate,
		LanguageCode:            g.languageCode,
		EnableAutomaticPunctuation: true,
		EnableWordTimeOffsets:   true,
		EnableSpokenPunctuation: wrapperspb.Bool(true),
	}
	if phraseSet != nil {
		config.Adaptation = &speechpb.SpeechAdaptation{
			PhraseSets: []*speechpb.PhraseSet{phraseSet},
		}
	}

	diarizationConfig := &speechpb.SpeakerDiarizationConfig{
		EnableSpeakerDiarization: false,
		MinSpeakerCount:          1,
		MaxSpeakerCount:          1,
	}
	config.SpeakerDiarizationConfig = diarizationConfig

	streamingConfig := &speechpb.StreamingRecognitionConfig{
		Config:                  config,
		InterimResults:          true,
		SingleUtterance:         false,
		EnableVoiceActivityEvents: true,
		VoiceActivityTimeout: &speechpb.StreamingRecognitionConfig_VoiceActivityTimeout{
			SpeechStartTimeout: durationpb.New(30 * time.Second),
			SpeechEndTimeout:   durationpb.New(500 * time.Millisecond),
		},
	}

	return g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: streamingConfig,
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
