package agency

import (
	"context"
	"errors"
	"testing"

	"synapta/config"
	"synapta/internal/modules/model"
)

// fakeBridgeModels es el resolver de las GPUs emparejadas del tenant para los
// tests: mismo contrato que el adapter del runtime (una búsqueda por nombre).
type fakeBridgeModels struct {
	ref      *BridgeModelRef
	err      error
	calls    int
	lastName string
}

func (f *fakeBridgeModels) resolve(_ context.Context, name string) (*BridgeModelRef, error) {
	f.calls++
	f.lastName = name
	return f.ref, f.err
}

func newBridgeCore(resolve BridgeModelResolver) *Core {
	c := NewCore(&config.Config{}, nil, nil, "", "")
	c.SetBridgeModels(resolve)
	return c
}

// Un modelo que sólo vive en la GPU emparejada del usuario debe resolverse:
// antes el core respondía "model not found" y la generación moría antes de
// llegar al generator (que sí sabe encolarla).
func TestLookupModelFallsBackToBridge(t *testing.T) {
	f := &fakeBridgeModels{ref: &BridgeModelRef{
		Name: "SDXL-Turbo", Label: "SDXL Turbo", Modality: model.ModalityImage,
		WorkerID: "worker-a", WorkerName: "PC-Casa", Available: true,
	}}
	c := newBridgeCore(f.resolve)

	got := c.lookupModel("SDXL-Turbo", "")
	if got == nil {
		t.Fatal("bridge model not resolved")
	}
	if got.Name != "SDXL-Turbo" || got.Type != model.TypeDownloaded || got.Modality != model.ModalityImage {
		t.Fatalf("resolved %+v, want downloaded image SDXL-Turbo", got)
	}
	if f.lastName != "SDXL-Turbo" {
		t.Fatalf("resolver asked for %q", f.lastName)
	}
}

// Un modelo declarado por la GPU sin pesos se resuelve igual: el generator
// explica que hay que bajarlo (Validate) en vez de decir que no existe.
func TestLookupModelResolvesBridgeModelWithoutWeights(t *testing.T) {
	c := newBridgeCore((&fakeBridgeModels{ref: &BridgeModelRef{
		Name: "FLUX.1-dev", Modality: model.ModalityImage, Available: false,
	}}).resolve)

	if got := c.lookupModel("FLUX.1-dev", ""); got == nil || got.Modality != model.ModalityImage {
		t.Fatalf("resolved %+v, want the declared model", got)
	}
}

func TestLookupModelUnknownNameStaysNil(t *testing.T) {
	f := &fakeBridgeModels{} // ninguna GPU declara el nombre
	c := newBridgeCore(f.resolve)

	if got := c.lookupModel("Nope-1", ""); got != nil {
		t.Fatalf("resolved %+v, want nil", got)
	}
	if f.calls != 1 {
		t.Fatalf("bridge resolver called %d times, want 1", f.calls)
	}
}

// El catálogo API manda: un modelo del código no consulta a las GPUs.
func TestLookupModelPrefersStaticCatalog(t *testing.T) {
	const apiModel = "dreamina-seedance-2-0-260128"
	f := &fakeBridgeModels{ref: &BridgeModelRef{Name: apiModel}}
	c := newBridgeCore(f.resolve)

	got := c.lookupModel(apiModel, "")
	if got == nil || got.Type != model.TypeAPI {
		t.Fatalf("resolved %+v, want the API catalog entry", got)
	}
	if f.calls != 0 {
		t.Fatalf("bridge resolver called %d times for an API model", f.calls)
	}
}

// Un nombre "<proveedor local>/<modelo>" se resuelve sin catálogo estático:
// el tenant puede levantar cualquier modelo en su Ollama/LM Studio. No consulta
// al worker ni a las GPUs.
func TestLookupModelResolvesLocalProviderNames(t *testing.T) {
	f := &fakeBridgeModels{}
	c := newBridgeCore(f.resolve)

	got := c.lookupModel("ollama/qwen2.5:7b", "")
	if got == nil {
		t.Fatal("local model not resolved")
	}
	if got.CredentialProvider != model.ProviderOllama {
		t.Errorf("provider = %q", got.CredentialProvider)
	}
	if got.Modality != model.ModalityText || got.Generator != "openai-text" {
		t.Errorf("modality/generator = %q/%q", got.Modality, got.Generator)
	}
	if got.Endpoint != "/chat/completions" {
		t.Errorf("endpoint = %q", got.Endpoint)
	}
	if got.BaseURL != model.DefaultLocalBaseURL(model.ProviderOllama) {
		t.Errorf("base_url = %q", got.BaseURL)
	}
	if f.calls != 0 {
		t.Fatalf("bridge resolver called %d times for a local model", f.calls)
	}

	if lm := c.lookupModel("lmstudio/qwen2.5-7b-instruct", ""); lm == nil || lm.CredentialProvider != model.ProviderLMStudio {
		t.Fatalf("lmstudio model resolved as %+v", lm)
	}
}

// Sin bridge (deploy con worker central) el comportamiento no cambia.
func TestLookupModelWithoutBridgeSource(t *testing.T) {
	c := NewCore(&config.Config{}, nil, nil, "", "")
	if got := c.lookupModel("SDXL-Turbo", ""); got != nil {
		t.Fatalf("resolved %+v without a bridge source, want nil", got)
	}
}

// Una falla del registry no debe inventar un modelo ni romper la generación
// con un error opaco: se trata como "no está en las GPUs".
func TestLookupModelBridgeErrorIsNotAModel(t *testing.T) {
	c := newBridgeCore(func(context.Context, string) (*BridgeModelRef, error) {
		return nil, errors.New("registry down")
	})
	if got := c.lookupModel("SDXL-Turbo", ""); got != nil {
		t.Fatalf("resolved %+v on registry failure, want nil", got)
	}
}
