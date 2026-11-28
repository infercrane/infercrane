package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// MaxKubernetesTolerations bounds how many taints an operator can ask model
// server pods to tolerate.
const MaxKubernetesTolerations = 16

// KubernetesToleration lets InferCrane's model server pods schedule onto nodes
// that carry a matching taint, for example a GPU node pool tainted
// nvidia.com/gpu:NoSchedule. It is the part of a Pod toleration the provider
// accepts: one named taint key, an explicit operator, and an explicit effect.
// A toleration for every key or every effect, and tolerationSeconds, are
// refused.
type KubernetesToleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect"`
}

// ParseKubernetesTolerations reads the operator's setting: a JSON array of
// tolerations in the Kubernetes field names, such as
// [{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}].
// An empty setting means no tolerations. Unknown fields are refused, so a
// field the provider would not render is never silently dropped.
func ParseKubernetesTolerations(raw string) ([]KubernetesToleration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var tolerations []KubernetesToleration
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tolerations); err != nil {
		return nil, fmt.Errorf("tolerations must be a JSON array of objects with key, operator, effect, and an optional value: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("tolerations must contain exactly one JSON array")
	}
	if err := ValidateKubernetesTolerations(tolerations); err != nil {
		return nil, err
	}
	if len(tolerations) == 0 {
		return nil, nil
	}
	return tolerations, nil
}

// ValidateKubernetesTolerations refuses anything but a bounded list of
// distinct tolerations, each for one named taint key with operator Exists or
// Equal and effect NoSchedule, PreferNoSchedule, or NoExecute.
func ValidateKubernetesTolerations(tolerations []KubernetesToleration) error {
	if len(tolerations) > MaxKubernetesTolerations {
		return fmt.Errorf("at most %d Kubernetes tolerations are accepted, got %d", MaxKubernetesTolerations, len(tolerations))
	}
	seen := make(map[KubernetesToleration]struct{}, len(tolerations))
	for index, toleration := range tolerations {
		if err := toleration.validate(); err != nil {
			return fmt.Errorf("Kubernetes toleration %d: %w", index+1, err)
		}
		if _, duplicate := seen[toleration]; duplicate {
			return fmt.Errorf("Kubernetes toleration %d repeats an earlier one", index+1)
		}
		seen[toleration] = struct{}{}
	}
	return nil
}

func (t KubernetesToleration) validate() error {
	if !validKubernetesQualifiedName(t.Key) {
		return fmt.Errorf("key %q must be a taint key such as nvidia.com/gpu; a toleration for every taint is not accepted", t.Key)
	}
	switch t.Operator {
	case "Exists":
		if t.Value != "" {
			return errors.New("operator Exists takes no value")
		}
	case "Equal":
		if !validKubernetesLabelValue(t.Value) {
			return fmt.Errorf("value %q is not a valid taint value", t.Value)
		}
	default:
		return fmt.Errorf("operator %q must be Exists or Equal", t.Operator)
	}
	switch t.Effect {
	case "NoSchedule", "PreferNoSchedule", "NoExecute":
	default:
		return fmt.Errorf("effect %q must be NoSchedule, PreferNoSchedule, or NoExecute", t.Effect)
	}
	return nil
}

// podTolerations renders the tolerations in one canonical order, so the
// manifest and the intent hash do not change when the operator reorders the
// setting. It returns nil when none are configured, and the manifest then has
// no tolerations field at all.
func (k Kubernetes) podTolerations() []KubernetesToleration {
	if len(k.Tolerations) == 0 {
		return nil
	}
	sorted := append([]KubernetesToleration(nil), k.Tolerations...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Effect != b.Effect {
			return a.Effect < b.Effect
		}
		if a.Operator != b.Operator {
			return a.Operator < b.Operator
		}
		return a.Value < b.Value
	})
	return sorted
}

// validKubernetesQualifiedName accepts a Kubernetes qualified name: an
// optional DNS subdomain prefix and a slash, then a name of at most 63
// letters, digits, '-', '_' and '.' that starts and ends with a letter or
// digit. Taint keys and label keys share this form.
func validKubernetesQualifiedName(value string) bool {
	name := value
	if index := strings.Index(value, "/"); index >= 0 {
		prefix := value[:index]
		name = value[index+1:]
		if prefix == "" || len(prefix) > 253 {
			return false
		}
		for _, label := range strings.Split(prefix, ".") {
			if !validKubernetesSubdomainLabel(label) {
				return false
			}
		}
	}
	return name != "" && validKubernetesLabelValue(name)
}

func validKubernetesSubdomainLabel(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		alphanumeric := (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')
		if alphanumeric || (char == '-' && index > 0 && index < len(value)-1) {
			continue
		}
		return false
	}
	return true
}

// validKubernetesLabelValue accepts an empty value or at most 63 letters,
// digits, '-', '_' and '.' that start and end with a letter or digit.
func validKubernetesLabelValue(value string) bool {
	if len(value) > 63 {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		alphanumeric := (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
		if alphanumeric || ((char == '-' || char == '_' || char == '.') && index > 0 && index < len(value)-1) {
			continue
		}
		return false
	}
	return true
}
