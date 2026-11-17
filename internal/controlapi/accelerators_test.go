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
			RuntimeImages []struct{ Runtime, Platform, Image, EngineVersion string } `json:"runtime_images"`
			Placement     []struct {
				Cloud, Mode string
				Platforms   []string
			} `json:"placement"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	data := body.Data
	if data.DefaultPlatform != "cuda" || len(data.Accelerators) != 4 || len(data.RuntimeImages) != 6 {
		t.Fatalf("catalog=%+v", data)
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
	unauthenticated := httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret"}).Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/v1/catalog/accelerators", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", unauthenticated.Code)
	}
}
