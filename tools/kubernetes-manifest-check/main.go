package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("manifest paths are required"))
	}
	seenRoute, seenDeployment, seenService, seenPVCRead, seenKServeRole, seenDynamoRole := false, false, false, false, false, false
	for _, path := range os.Args[1:] {
		file, err := os.Open(path)
		if err != nil {
			fatal(err)
		}
		decoder := yaml.NewDecoder(file)
		for {
			var document map[string]any
			err = decoder.Decode(&document)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				file.Close()
				fatal(fmt.Errorf("%s: %w", path, err))
			}
			if len(document) == 0 {
				continue
			}
			kind, _ := document["kind"].(string)
			switch kind {
			case "Namespace", "ServiceAccount", "RoleBinding":
			case "Role":
				resources, checkErr := checkRole(document)
				if checkErr != nil {
					file.Close()
					fatal(fmt.Errorf("%s: %w", path, checkErr))
				}
				for _, resource := range resources {
					seenDeployment = seenDeployment || resource == "deployments"
					seenService = seenService || resource == "services"
					seenPVCRead = seenPVCRead || resource == "persistentvolumeclaims"
					seenKServeRole = seenKServeRole || resource == "inferenceservices"
					seenDynamoRole = seenDynamoRole || resource == "dynamographdeployments"
				}
			case "HTTPRoute":
				if err = checkHTTPRoute(document); err != nil {
					file.Close()
					fatal(fmt.Errorf("%s: %w", path, err))
				}
				seenRoute = true
			default:
				file.Close()
				fatal(fmt.Errorf("%s: unexpected kind %q", path, kind))
			}
		}
		if err = file.Close(); err != nil {
			fatal(err)
		}
	}
	if !seenRoute || !seenDeployment || !seenService || !seenPVCRead || !seenKServeRole || !seenDynamoRole {
		fatal(errors.New("manifests do not cover deployment RBAC, read-only PVC evidence, KServe RBAC, Dynamo RBAC, and Gateway API exposure"))
	}
	fmt.Println("Kubernetes manifests are syntactically valid and preserve bounded ownership.")
}

func checkRole(document map[string]any) ([]string, error) {
	rules, _ := document["rules"].([]any)
	if len(rules) == 0 {
		return nil, errors.New("Role has no rules")
	}
	var out []string
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		resources := stringSlice(rule["resources"])
		verbs := stringSlice(rule["verbs"])
		for _, value := range append(append([]string{}, resources...), verbs...) {
			if value == "*" {
				return nil, errors.New("wildcard RBAC is forbidden")
			}
		}
		for _, resource := range resources {
			if resource == "secrets" || resource == "pods" {
				return nil, fmt.Errorf("control plane must not receive %s access", resource)
			}
			out = append(out, resource)
		}
		requiredVerbs := []string{"get", "list", "create", "patch", "delete"}
		if len(resources) == 1 && resources[0] == "persistentvolumeclaims" {
			requiredVerbs = []string{"get", "list"}
			for _, forbidden := range []string{"create", "patch", "update", "delete", "deletecollection"} {
				if contains(verbs, forbidden) {
					return nil, fmt.Errorf("PVC evidence RBAC must not include %s", forbidden)
				}
			}
		}
		for _, required := range requiredVerbs {
			if !contains(verbs, required) {
				return nil, fmt.Errorf("Role is missing %s", required)
			}
		}
	}
	return out, nil
}

func checkHTTPRoute(document map[string]any) error {
	encoded, _ := yaml.Marshal(document)
	text := string(encoded)
	if !strings.Contains(text, "name: infercrane-gateway") || !strings.Contains(text, "port: 8080") {
		return errors.New("HTTPRoute must target only the InferCrane gateway Service on port 8080")
	}
	for _, forbidden := range []string{"weight:", "InferencePool", "LLMInferenceService"} {
		if strings.Contains(text, forbidden) {
			return fmt.Errorf("HTTPRoute contains forbidden inference-routing field %q", forbidden)
		}
	}
	return nil
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
