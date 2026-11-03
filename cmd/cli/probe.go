package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/cli/args"
	"github.com/tsic404/rffmpeg/pkg/cli/client"
)

// runProbe handles the "probe" subcommand.
func runProbe(cli *client.Client, input string, quiet bool, sharedFS bool) int {
	if input == "" {
		fmt.Fprintln(os.Stderr, "Error: probe requires an input file or URL")
		fmt.Fprintln(os.Stderr, "Usage: rffmpeg probe <file|URL>")
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Probing %s...\n", input)
	}

	if strings.HasPrefix(input, "file://") {
		filePath, _ := args.StripFileScheme(input)
		absPath, err := args.GetAbsPath(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", input, err)
			return ExitError
		}

		if err := args.ValidateFileExists(absPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error validating input %s: %v\n", input, err)
			return ExitError
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Using local file: %s\n", absPath)
		}

		if sharedFS {
			return runProbeRequest(cli, absPath, quiet)
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Uploading %s...\n", absPath)
		}
		fileID, err := cli.UploadFileAuto(absPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error uploading %s: %v\n", absPath, err)
			return ExitError
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Uploaded: %s -> %s\n", absPath, fileID)
		}
		return runProbeRequest(cli, fileID, quiet)
	}

	if strings.Contains(input, "://") {
		if !quiet {
			fmt.Fprintf(os.Stderr, "Using remote URL: %s\n", input)
		}
		return runProbeRequest(cli, input, quiet)
	}

	if sharedFS {
		absPath, err := args.GetAbsPath(input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", input, err)
			return ExitError
		}

		if err := args.ValidateFileExists(absPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return ExitError
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "Shared FS: using direct path %s\n", absPath)
		}
		return runProbeRequest(cli, absPath, quiet)
	}

	absPath, err := args.GetAbsPath(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", input, err)
		return ExitError
	}

	if err := args.ValidateFileExists(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Uploading %s...\n", input)
	}

	fileID, err := cli.UploadFileAuto(absPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error uploading %s: %v\n", input, err)
		return ExitError
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Uploaded: %s -> %s\n", input, fileID)
	}

	return runProbeRequest(cli, fileID, quiet)
}

// runProbeRequest sends a probe request to the server and outputs the result.
func runProbeRequest(cli *client.Client, input string, quiet bool) int {
	if !quiet {
		fmt.Fprintln(os.Stderr, "Running ffprobe...")
	}

	result, err := cli.Probe(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error probing file: %v\n", err)
		return ExitError
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding probe result: %v\n", err)
		return ExitError
	}

	return ExitSuccess
}
