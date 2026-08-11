package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"

	"github.com/infercrane/infercrane/internal/config"
)

func adoptCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) >= 2 && args[0] == "promote" {
		fs := flag.NewFlagSet("adopt promote", flag.ContinueOnError)
		ownership := fs.String("ownership", "traffic-managed", "traffic-managed")
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: infercrane adopt promote NAME --ownership traffic-managed")
		}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodPut, "/api/v1/adoptions/endpoints/"+url.PathEscape(args[1])+"/ownership", "", map[string]string{"ownership_mode": *ownership}, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("endpoint %s promoted to traffic-managed; provider lifecycle remains external\n", args[1])
		return nil
	}
	if len(args) < 2 || args[0] != "endpoint" {
		return errors.New("usage: infercrane adopt endpoint NAME --url URL --model LOGICAL_MODEL [--ownership observe-only]")
	}
	name := args[1]
	fs := flag.NewFlagSet("adopt endpoint", flag.ContinueOnError)
	rawURL := fs.String("url", "", "existing OpenAI-compatible base URL")
	model := fs.String("model", "", "stable logical model name")
	upstreamModel := fs.String("upstream-model", "", "physical model name exposed by the workload")
	source := fs.String("source", "vllm", "vllm or openai-compatible")
	ownership := fs.String("ownership", "observe-only", "observe-only or traffic-managed")
	runtimeName := fs.String("runtime", "vllm", "qualified runtime inspector")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || name == "" || *rawURL == "" || *model == "" {
		return errors.New("usage: infercrane adopt endpoint NAME --url URL --model LOGICAL_MODEL [--ownership observe-only]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	request := map[string]string{"name": name, "logical_model": *model, "upstream_model": *upstreamModel, "url": *rawURL, "source": *source, "ownership_mode": *ownership, "runtime": *runtimeName}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/adoptions/endpoints", "", request, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("endpoint %s adopted · ownership %s · no provider lifecycle transferred\n", name, *ownership)
	if *ownership == "observe-only" {
		fmt.Println("traffic is unchanged; promote ownership to traffic-managed explicitly before routing through InferCrane")
	}
	return nil
}
