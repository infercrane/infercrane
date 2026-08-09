package spec

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Deployment struct {
	Name  string `yaml:"name"`
	Model struct {
		ID string `yaml:"id"`
	} `yaml:"model"`
	Runtime struct {
		Engine string   `yaml:"engine"`
		Args   []string `yaml:"args"`
	} `yaml:"runtime"`
	Resources struct {
		GPU string `yaml:"gpu"`
	} `yaml:"resources"`
	Provider struct {
		Cloud  string `yaml:"cloud"`
		Region string `yaml:"region"`
	} `yaml:"provider"`
	Scaling struct {
		MinReplicas int `yaml:"min_replicas"`
		MaxReplicas int `yaml:"max_replicas"`
	} `yaml:"scaling"`
	Routing struct {
		Strategy string `yaml:"strategy"`
	} `yaml:"routing"`
}

func Load(path string) (Deployment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Deployment{}, fmt.Errorf("read deployment file: %w", err)
	}
	var out Deployment
	if err = yaml.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("parse deployment file: %w", err)
	}
	if out.Runtime.Engine == "" {
		out.Runtime.Engine = "vllm"
	}
	if out.Scaling.MinReplicas == 0 {
		out.Scaling.MinReplicas = 1
	}
	if out.Scaling.MaxReplicas == 0 {
		out.Scaling.MaxReplicas = 1
	}
	if out.Routing.Strategy == "" {
		out.Routing.Strategy = "round-robin"
	}
	if out.Name == "" || out.Model.ID == "" || out.Resources.GPU == "" || out.Provider.Cloud == "" {
		return out, fmt.Errorf("name, model.id, resources.gpu, and provider.cloud are required")
	}
	if out.Scaling.MaxReplicas < out.Scaling.MinReplicas {
		return out, fmt.Errorf("scaling.max_replicas must be >= scaling.min_replicas")
	}
	return out, nil
}
