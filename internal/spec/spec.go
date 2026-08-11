package spec

import (
	"fmt"
	"os"

	"github.com/infercrane/infercrane/internal/support"
	"gopkg.in/yaml.v3"
)

type Deployment struct {
	Name  string `yaml:"name"`
	Model struct {
		ID       string `yaml:"id"`
		Revision string `yaml:"revision"`
	} `yaml:"model"`
	Runtime struct {
		Engine  string   `yaml:"engine"`
		Version string   `yaml:"version"`
		Args    []string `yaml:"args"`
	} `yaml:"runtime"`
	Compute struct {
		Mode string `yaml:"mode"`
	} `yaml:"compute"`
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
	if out.Compute.Mode == "" {
		out.Compute.Mode = "elastic"
	}
	if out.Compute.Mode != "elastic" && out.Compute.Mode != "serverless" {
		return out, fmt.Errorf("compute.mode must be elastic or serverless")
	}
	if out.Compute.Mode == "elastic" && out.Scaling.MinReplicas == 0 {
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
	if err := support.V03().Validate(out.Runtime.Engine, out.Provider.Cloud, out.Compute.Mode); err != nil {
		return out, fmt.Errorf("support policy: %w", err)
	}
	if out.Provider.Cloud == "aws" && out.Provider.Region == "" {
		return out, fmt.Errorf("AWS BYOC requires provider.region")
	}
	if out.Scaling.MaxReplicas < out.Scaling.MinReplicas {
		return out, fmt.Errorf("scaling.max_replicas must be >= scaling.min_replicas")
	}
	if out.Compute.Mode == "serverless" && out.Scaling.MinReplicas != 0 {
		return out, fmt.Errorf("serverless compute requires scaling.min_replicas 0")
	}
	return out, nil
}
