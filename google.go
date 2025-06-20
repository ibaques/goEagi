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

// GoogleResult contiene el resultado de la transcripción o error.
type GoogleResult struct {
	Result            *speechpb.StreamingRecognizeResponse
	Error             error
	TotalBilledTime   time.Duration
	Reinitialized     bool
	ReinitializedInfo string
}

// GoogleService gestiona el streaming a Google Speech-to-Text.
type GoogleService struct {
	languageCode   string
	privateKeyPath string
	enhancedMode   bool
	domainModel    string
	speechContext  []string
	client         speech.Speech_StreamingRecognizeClient

	sync.RWMutex
}

// NewGoogleService crea el cliente para Google Speech-to-Text v2.
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

	g.client, err = client.StreamingRecognize(ctx)
	if err != nil {
		return nil, err
	}

	// Construir PhraseSet para Adaptation
	phraseSet := &speechpb.SpeechAdaptation_AdaptationPhraseSet{
		PhraseSetId: "custom_phrases",
		PhraseSet: &speechpb.PhraseSet{
			Phrases: make([]*speechpb.PhraseSet_Phrase, len(g.speechContext)),
		},
	}
	for i, phrase := range g.speechContext {
		phraseSet.PhraseSet.Phrases[i] = &speechpb.PhraseSet_Phrase{Value: phrase}
	}

	config := &speechpb.RecognitionConfig{
		Encoding:                   speechpb.AudioEncoding_LINEAR16,
		SampleRateHertz:            sampleRate,
		LanguageCode:               g.languageCode,
		Model:                     g.domainModel,
		UseEnhanced:               g.enhancedMode,
		EnableAutomaticPunctuation: true,
		EnableWordTimeOffsets:      true,
		EnableSpokenPunctuation:    wrapperspb.Bool(true),
		Adaptation: &speechpb.SpeechAdaptation{
			PhraseSets: []*speechpb.SpeechAdaptation_AdaptationPhraseSet{phraseSet},
		},
		// Diarization removido según pedido
	}

	streamingConfig := &speechpb.StreamingRecognitionConfig{
		Config:                  config,
		InterimResults:          true,
		SingleUtterance:         false,
		EnableVoiceActivityEvents: true,
		VoiceActivityTimeout: &speechpb.StreamingRecognitionConfig_VoiceActivityTimeout{
			SpeechStartTimeout: durationpb.New(30 * time.Second),       // Espera max 30 seg para que inicie voz
			SpeechEndTimeout:   durationpb.New(500 * time.Millisecond),  // Detecta fin voz con 0.5s de silencio
		},
	}

	if err := g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: streamingConfig,
		},
	}); err != nil {
		return nil, err
	}

	return &g, nil
}

// StartStreaming envía audio al streaming de Google.
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

// SpeechToTextResponse recibe las respuestas de transcripción.
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

// Close cierra la conexión con el cliente Google.
func (g *GoogleService) Close() error {
	g.Lock()
	defer g.Unlock()
	return g.client.CloseSend()
}

// ReinitializeClient reinicia el cliente de Google.
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

	// Reconstruir PhraseSet para Adaptation
	phraseSet := &speechpb.SpeechAdaptation_AdaptationPhraseSet{
		PhraseSetId: "custom_phrases",
		PhraseSet: &speechpb.PhraseSet{
			Phrases: make([]*speechpb.PhraseSet_Phrase, len(g.speechContext)),
		},
	}
	for i, phrase := range g.speechContext {
		phraseSet.PhraseSet.Phrases[i] = &speechpb.PhraseSet_Phrase{Value: phrase}
	}

	config := &speechpb.RecognitionConfig{
		Encoding:                   speechpb.AudioEncoding_LINEAR16,
		SampleRateHertz:            sampleRate,
		LanguageCode:               g.languageCode,
		Model:                     g.domainModel,
		UseEnhanced:               g.enhancedMode,
		EnableAutomaticPunctuation: true,
		EnableWordTimeOffsets:      true,
		EnableSpokenPunctuation:    wrapperspb.Bool(true),
		Adaptation: &speechpb.SpeechAdaptation{
			PhraseSets: []*speechpb.SpeechAdaptation_AdaptationPhraseSet{phraseSet},
		},
	}

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

	if err := g.client.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: streamingConfig,
		},
	}); err != nil {
		return err
	}

	return nil
}

// Listas de idiomas soportados

func supportedEnhancedMode() []string {
	return []string{"es-US", "en-GB", "en-US", "fr-FR", "ja-JP", "pt-BR", "ru-RU", "es-ES"}
}

func supportedTelephony() []string {
	return []string{"pt-PT", "nl-NL"}
}

func supportedDefault() []string {
	return []string{"ca-ES", "da-DK"}
}
