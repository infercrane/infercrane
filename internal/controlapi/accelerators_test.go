package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestAcceleratorCatalogNamesAMDIdentityImagesAndPlacementWithoutMeasurementClaims(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/accelerators", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Data struct {
			DefaultPlatform string `json:"default_platform"`
			Accelerators    []struct {
				Name, Vendor, Platform, Target string
				MemoryGiB                      int               `json:"memory_gib"`
				ProviderIDs                    map[string]string `json:"provider_ids"`
				Measured                       bool              `json:"measured"`
				Limitations                    []string          `json:"limitations"`
				Sources                        []string          `json:"sources"`
			} `json:"accelerators"`
			RuntimeImages []struct {
				Runtime, Platform, Image, Status, Note string
				EngineVersion                          string `json:"engine_version"`
			} `json:"runtime_images"`
			Placement []struct {
				Cloud, Mode string
				Platforms   []string
			} `json:"placement"`
			NotQualified []struct {
				Name, Vendor, Platform, Status string
				DeviceResource                 string   `json:"device_resource"`
				NativeFP8                      bool     `json:"native_fp8"`
				Limitations                    []string `json:"limitations"`
				CandidateImages                []struct {
					Platform, Image, Status string
				} `json:"candidate_images"`
			} `json:"not_qualified"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	data := body.Data
	if data.DefaultPlatform != "cuda" || len(data.Accelerators) != 4 || len(data.RuntimeImages) != 12 {
		t.Fatalf("catalog=%+v", data)
	}
	// vLLM 0.34.0 and SGLang 0.5.23 are current; vLLM 0.22.1 and SGLang
	// 0.5.12 are listed as legacy, with why they are kept and how to move.
	statuses := map[string]int{}
	for _, image := range data.RuntimeImages {
		statuses[image.EngineVersion+" "+image.Status]++
		if image.Status == "legacy" && !strings.Contains(image.Note, "0.34.0") && !strings.Contains(image.Note, "0.5.23") {
			t.Fatalf("legacy image without a move note: %+v", image)
		}
	}
	if statuses["0.34.0 current"] != 3 || statuses["0.22.1 legacy"] != 3 || statuses["0.5.23 current"] != 3 || statuses["0.5.12 legacy"] != 3 {
		t.Fatalf("image statuses=%v", statuses)
	}
	mi300x := data.Accelerators[0]
	if mi300x.Name != "MI300X" || mi300x.Vendor != "amd" || mi300x.Platform != "rocm-gfx942" || mi300x.MemoryGiB != 192 || mi300x.ProviderIDs["runpod"] != "AMD Instinct MI300X OAM" || mi300x.Measured || len(mi300x.Sources) == 0 || !strings.Contains(strings.Join(mi300x.Limitations, " "), "No model has been served or measured") {
		t.Fatalf("MI300X=%+v", mi300x)
	}
	if mi355x := data.Accelerators[3]; mi355x.ProviderIDs["runpod"] != "" || !strings.Contains(strings.Join(mi355x.Limitations, " "), "RunPod does not list") {
		t.Fatalf("MI355X=%+v", mi355x)
	}
	for _, placement := range data.Placement {
		amd := slices.Contains(placement.Platforms, "rocm-gfx942")
		if want := (placement.Cloud == "runpod" && placement.Mode == "elastic") || placement.Cloud == "kubernetes"; amd != want {
			t.Fatalf("placement %+v", placement)
		}
	}
	// TPU and Intel accelerators are listed apart, never among the
	// accelerators or images a spec may select, each with the builds a first
	// qualification run would pin.
	vendors := map[string]int{}
	for _, accelerator := range data.Accelerators {
		if accelerator.Vendor != "amd" {
			t.Fatalf("selectable accelerators include %+v", accelerator)
		}
	}
	for _, image := range data.RuntimeImages {
		if strings.HasPrefix(image.Platform, "tpu-") || strings.HasPrefix(image.Platform, "hpu-") || strings.HasPrefix(image.Platform, "xpu-") {
			t.Fatalf("selectable images include %+v", image)
		}
	}
	for _, accelerator := range data.NotQualified {
		vendors[accelerator.Vendor]++
		if accelerator.Status != "not-qualified" || accelerator.NativeFP8 || accelerator.DeviceResource == "" || len(accelerator.CandidateImages) == 0 || !strings.Contains(strings.Join(accelerator.Limitations, " "), "refuses every spec") {
			t.Fatalf("not-qualified entry %+v", accelerator)
		}
		for _, image := range accelerator.CandidateImages {
			if image.Status != "not-qualified" || image.Platform != accelerator.Platform || !strings.Contains(image.Image, "@sha256:") {
				t.Fatalf("%s candidate image %+v", accelerator.Name, image)
			}
		}
		for _, placement := range data.Placement {
			if slices.Contains(placement.Platforms, accelerator.Platform) {
				t.Fatalf("%s is placed on %+v", accelerator.Name, placement)
			}
		}
	}
	if vendors["google"] != 4 || vendors["intel"] != 4 {
		t.Fatalf("not-qualified vendors=%v", vendors)
	}
	unauthenticated := httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret"}).Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/v1/catalog/accelerators", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauthenticated.Code)
	}
}
