package provision_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/provision"
)

func TestParseKubernetesTolerationsAcceptsTheKubernetesFieldNames(t *testing.T) {
	for _, unset := range []string{"", "  ", "[]", "null"} {
		if tolerations, err := provision.ParseKubernetesTolerations(unset); err != nil || tolerations != nil {
			t.Fatalf("%q: tolerations=%#v err=%v", unset, tolerations, err)
		}
	}
	tolerations, err := provision.ParseKubernetesTolerations(`
		[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"},
		 {"key":"dedicated","operator":"Equal","value":"inference","effect":"NoExecute"},
		 {"key":"spot","operator":"Equal","effect":"PreferNoSchedule"}]`)
	want := []provision.KubernetesToleration{
		{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"},
		{Key: "dedicated", Operator: "Equal", Value: "inference", Effect: "NoExecute"},
		{Key: "spot", Operator: "Equal", Effect: "PreferNoSchedule"},
	}
	if err != nil || !reflect.DeepEqual(tolerations, want) {
		t.Fatalf("tolerations=%#v err=%v", tolerations, err)
	}
}

func TestParseKubernetesTolerationsRefusesEverythingElse(t *testing.T) {
	for name, raw := range map[string]string{
		"not JSON":           "nvidia.com/gpu:NoSchedule",
		"an object":          `{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}`,
		"strings":            `["nvidia.com/gpu:NoSchedule"]`,
		"a number as key":    `[{"key":5,"operator":"Exists","effect":"NoSchedule"}]`,
		"toleration seconds": `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoExecute","tolerationSeconds":60}]`,
		"an unknown field":   `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule","namespace":"kube-system"}]`,
		"two arrays":         `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}] []`,
		"no operator":        `[{"key":"nvidia.com/gpu","effect":"NoSchedule"}]`,
		"no effect":          `[{"key":"nvidia.com/gpu","operator":"Exists"}]`,
		"no key":             `[{"operator":"Exists","effect":"NoSchedule"}]`,
		"value with Exists":  `[{"key":"nvidia.com/gpu","operator":"Exists","value":"present","effect":"NoSchedule"}]`,
	} {
		if tolerations, err := provision.ParseKubernetesTolerations(raw); err == nil {
			t.Errorf("%s was accepted: %#v", name, tolerations)
		}
	}
}

func TestKubernetesTolerationCountIsBounded(t *testing.T) {
	entries := make([]string, 0, provision.MaxKubernetesTolerations+1)
	for index := 0; index <= provision.MaxKubernetesTolerations; index++ {
		entries = append(entries, fmt.Sprintf(`{"key":"pool-%d","operator":"Exists","effect":"NoSchedule"}`, index))
	}
	atLimit := "[" + strings.Join(entries[:provision.MaxKubernetesTolerations], ",") + "]"
	if tolerations, err := provision.ParseKubernetesTolerations(atLimit); err != nil || len(tolerations) != provision.MaxKubernetesTolerations {
		t.Fatalf("%d tolerations: got %d, err=%v", provision.MaxKubernetesTolerations, len(tolerations), err)
	}
	overLimit := "[" + strings.Join(entries, ",") + "]"
	if _, err := provision.ParseKubernetesTolerations(overLimit); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("%d tolerations: err=%v", len(entries), err)
	}
}
