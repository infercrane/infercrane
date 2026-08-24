package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/infercrane/infercrane/internal/nodediscovery"
)

var discoverLocal = func(ctx context.Context) (nodediscovery.Report, error) {
	return nodediscovery.DiscoverLocal(ctx, nil)
}

func discoverCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "local" {
		return errors.New("usage: infercrane discover local [--output human|json]")
	}
	fs := flag.NewFlagSet("discover local", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane discover local [--output human|json]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	report, err := discoverLocal(ctx)
	if err != nil {
		return err
	}
	if *output == "json" {
		encoded, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Printf("Local accelerator discovery · %s\nContract  %s\nSource    %s\n\n", report.State, report.Contract, report.Source)
	if len(report.GPUs) > 0 {
		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "INDEX\tGPU\tMEMORY\tDRIVER\tUUID")
		for _, gpu := range report.GPUs {
			fmt.Fprintf(writer, "%d\t%s\t%d MiB\t%s\t%s\n", gpu.Index, gpu.Name, gpu.MemoryTotalMiB, gpu.DriverVersion, gpu.UUID)
		}
		if err = writer.Flush(); err != nil {
			return err
		}
	} else {
		fmt.Println("No concrete NVIDIA GPU inventory is available.")
	}
	for _, limitation := range report.Limitations {
		fmt.Printf("- %s\n", limitation)
	}
	fmt.Println("\nIf a runtime already serves this host, connect it without transferring lifecycle ownership:")
	fmt.Println("  infercrane connect http://HOST:PORT/v1 --as MODEL_NAME")
	return nil
}
