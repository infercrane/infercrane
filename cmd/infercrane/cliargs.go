package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
)

// newFlagSet returns a command flag set that reports problems only through
// the returned error. Go's default flag output prints "Usage of NAME:" to
// stderr, which corrupts JSON output and duplicates the error InferCrane
// already renders; command help comes from Cobra instead.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// commandSubject returns args[index] as the resource a command acts on. A
// missing subject, or a flag in its place, is a usage error, so
// "status --output json" never asks the control plane for a deployment named
// "--output".
func commandSubject(args []string, index int, usage string) (string, error) {
	if index >= len(args) || strings.HasPrefix(args[index], "-") {
		return "", errors.New(usage)
	}
	return args[index], nil
}

// parseSubjectFlags reads the subject at args[index] and parses the flags
// after it. A stray positional argument is a usage error rather than a point
// where flag parsing silently stops, which would drop a later --output.
func parseSubjectFlags(fs *flag.FlagSet, args []string, index int, usage string) (string, error) {
	subject, err := commandSubject(args, index, usage)
	if err != nil {
		return "", err
	}
	if err = parseFlagsOnly(fs, args[index+1:], usage); err != nil {
		return "", err
	}
	return subject, nil
}

// parseFlagsOnly parses a command that takes no further positional arguments.
func parseFlagsOnly(fs *flag.FlagSet, args []string, usage string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(usage)
	}
	return nil
}

// applyGlobalFlags consumes the global flags. --context and --output are read
// before the command; --no-color is accepted anywhere before a "--"
// terminator. A leading --output moves behind the command's own arguments,
// where every command parses it.
func applyGlobalFlags(args []string) ([]string, error) {
	var output []string
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--no-color":
			if err := os.Setenv("NO_COLOR", "1"); err != nil {
				return nil, err
			}
			args = args[1:]
		case arg == "--context":
			if len(args) < 2 || strings.HasPrefix(args[1], "-") {
				return nil, errors.New("--context requires a context name")
			}
			if err := os.Setenv("INFERCRANE_CONTEXT", args[1]); err != nil {
				return nil, err
			}
			args = args[2:]
		case arg == "--output":
			if len(args) < 2 || strings.HasPrefix(args[1], "-") {
				return nil, errors.New("--output must be human or json")
			}
			output, args = args[:2:2], args[2:]
		case strings.HasPrefix(arg, "--output="):
			output, args = args[:1:1], args[1:]
		default:
			return withGlobalFlags(args, output)
		}
	}
	return withGlobalFlags(args, output)
}

func withGlobalFlags(args, output []string) ([]string, error) {
	result := make([]string, 0, len(args)+len(output))
	terminator := -1
	for _, arg := range args {
		if terminator < 0 && arg == "--" {
			terminator = len(result)
		}
		if terminator < 0 && arg == "--no-color" {
			if err := os.Setenv("NO_COLOR", "1"); err != nil {
				return nil, err
			}
			continue
		}
		result = append(result, arg)
	}
	if len(output) == 0 {
		return result, nil
	}
	if terminator < 0 {
		return append(result, output...), nil
	}
	return append(append(append([]string{}, result[:terminator]...), output...), result[terminator:]...), nil
}

type versionInfo struct {
	Version string            `json:"version"`
	Build   map[string]string `json:"build"`
}

// currentVersionInfo reports the release version plus whatever build metadata
// the Go toolchain embedded. Missing VCS fields are omitted, not invented.
func currentVersionInfo() versionInfo {
	build := map[string]string{"go_version": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				build["commit"] = setting.Value
			case "vcs.time":
				build["commit_time"] = setting.Value
			case "vcs.modified":
				build["modified"] = setting.Value
			}
		}
	}
	return versionInfo{Version: version, Build: build}
}

func versionCommand(args []string) error {
	fs := newFlagSet("version")
	output := fs.String("output", "human", "human or json")
	if err := parseFlagsOnly(fs, args, "usage: infercrane version [--output human|json]"); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(currentVersionInfo())
	}
	fmt.Println(version)
	return nil
}
