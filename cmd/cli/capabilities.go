package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/cli/client"
	"github.com/tsic404/rffmpeg/pkg/cli/config"
	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// localFallbackEnv opts into answering a server-backed capability query from
// local ffmpeg when the server cannot answer it (see localFallbackAllowed).
const localFallbackEnv = "RFFMPEG_LOCAL_FALLBACK"

// localFallbackAllowed decides what a server-backed capability query does after
// the server failed to answer it. The capability list describes the cluster the
// job will run on, so answering with the local machine's list is a substitution
// the caller must ask for: without the opt-in the query fails, and with it the
// substitution is announced on stderr. Returns true when the caller should run
// local ffmpeg.
func localFallbackAllowed(flag string, serverErr error) bool {
	switch os.Getenv(localFallbackEnv) {
	case "1", "true":
		fmt.Fprintf(os.Stderr, "[rffmpeg] local fallback: %s server query failed (%v); listing LOCAL ffmpeg capabilities\n", flag, serverErr)
		return true
	}
	fmt.Fprintf(os.Stderr, "Error: %s server query failed: %v\n", flag, serverErr)
	fmt.Fprintf(os.Stderr, "Hint: this list comes from the cluster; set %s=1 to list the LOCAL ffmpeg's capabilities instead.\n", localFallbackEnv)
	return false
}

// runEncoders queries encoders from the server and prints in ffmpeg-compatible format.
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg only with the localFallbackEnv opt-in.
func runEncoders(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		encoders, apiErr := cli.ListAllEncoders()
		if apiErr == nil {
			if jsonOut {
				return printJSONEncoderInfo(encoders, "encoders")
			}
			if len(encoders) == 0 {
				fmt.Fprintf(os.Stderr, "No encoders available\n")
				return ExitSuccess
			}
			printEncoderDecoderHeader()
			for _, enc := range encoders {
				capFlags := encoderCapabilityFlags(enc.Type)
				fmt.Printf("%s %-22s %s\n", capFlags, enc.Name, enc.Description)
			}
			return ExitSuccess
		}
		err = apiErr
	}
	if !localFallbackAllowed("-encoders", err) {
		return ExitError
	}
	if !jsonOut {
		return runLocalFfmpegPassthrough("-encoders")
	}
	output, runErr := runLocalFfmpegOutput("-encoders")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -encoders: %v\n", runErr)
		return ExitError
	}
	encoders := parseFFmpegEncoderLines(output)
	return printJSONEncoderInfo(encoders, "encoders")
}

// runDecoders queries decoders from the server and prints in ffmpeg-compatible format.
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg only with the localFallbackEnv opt-in.
func runDecoders(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		decoders, apiErr := cli.ListAllDecoders()
		if apiErr == nil {
			if jsonOut {
				return printJSONDecoderInfo(decoders, "decoders")
			}
			if len(decoders) == 0 {
				fmt.Fprintf(os.Stderr, "No decoders available\n")
				return ExitSuccess
			}
			printEncoderDecoderHeader()
			for _, dec := range decoders {
				capFlags := decoderCapabilityFlags(dec.Type)
				fmt.Printf("%s %-22s %s\n", capFlags, dec.Name, dec.Description)
			}
			return ExitSuccess
		}
		err = apiErr
	}
	if !localFallbackAllowed("-decoders", err) {
		return ExitError
	}
	if !jsonOut {
		return runLocalFfmpegPassthrough("-decoders")
	}
	output, runErr := runLocalFfmpegOutput("-decoders")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -decoders: %v\n", runErr)
		return ExitError
	}
	decoders := parseFFmpegEncoderLines(output)

	decoderInfos := make([]protocol.DecoderInfo, len(decoders))
	for i, e := range decoders {
		decoderInfos[i] = protocol.DecoderInfo{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
		}
	}
	return printJSONDecoderInfo(decoderInfos, "decoders")
}

// runCodecsFromServer queries both encoders and decoders from the server and prints in
// ffmpeg-compatible format: encoder list first, then decoder list.
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg only with the localFallbackEnv opt-in.
func runCodecsFromServer(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		var encoders []protocol.EncoderInfo
		var decoders []protocol.DecoderInfo
		encoders, err = cli.ListAllEncoders()
		if err == nil {
			decoders, err = cli.ListAllDecoders()
		}
		if err == nil {
			if jsonOut {
				return outputInfoFlagJSON("codecs", encoders, decoders)
			}
			printCodecsHeader("Encoders")
			if len(encoders) > 0 {
				for _, enc := range encoders {
					capFlags := encoderCapabilityFlags(enc.Type)
					fmt.Printf("%s %-22s %s\n", capFlags, enc.Name, enc.Description)
				}
			} else {
				fmt.Fprintf(os.Stderr, "No encoders available\n")
			}

			printCodecsHeader("Decoders")
			if len(decoders) > 0 {
				for _, dec := range decoders {
					capFlags := decoderCapabilityFlags(dec.Type)
					fmt.Printf("%s %-22s %s\n", capFlags, dec.Name, dec.Description)
				}
			} else {
				fmt.Fprintf(os.Stderr, "No decoders available\n")
			}
			return ExitSuccess
		}
	}

	if !localFallbackAllowed("-codecs", err) {
		return ExitError
	}
	if !jsonOut {
		return runLocalFfmpegPassthrough("-codecs")
	}

	encOut, encErr := runLocalFfmpegOutput("-encoders")
	if encErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -encoders: %v\n", encErr)
		return ExitError
	}
	decOut, decErr := runLocalFfmpegOutput("-decoders")
	if decErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -decoders: %v\n", decErr)
		return ExitError
	}
	encoders := parseFFmpegEncoderLines(encOut)
	encDecoderLines := parseFFmpegEncoderLines(decOut)
	decoderInfos := make([]protocol.DecoderInfo, len(encDecoderLines))
	for i, e := range encDecoderLines {
		decoderInfos[i] = protocol.DecoderInfo{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
		}
	}
	return outputInfoFlagJSON("codecs", encoders, decoderInfos)
}

// runLocalFfmpegInfo runs a local ffmpeg info flag and prints output to stdout.
// Used for P2 info flags that don't have a server API endpoint.
func runLocalFfmpegInfo(flag string) int {
	cmd := exec.Command("ffmpeg", flag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running ffmpeg %s: %v\n", flag, err)
		return ExitError
	}
	return ExitSuccess
}

// runLocalFfmpegOutput runs a local ffmpeg with the given flag, capturing stdout
// and discarding stderr. Returns the combined output string.
func runLocalFfmpegOutput(flag string) (string, error) {
	cmd := exec.Command("ffmpeg", flag)
	cmd.Stderr = nil

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	return stdout.String(), err
}

// runLocalFfmpegPassthrough runs ffmpeg with the given flag, piping stdout and
// stderr directly to the parent process. Returns ffmpeg's exit code.
func runLocalFfmpegPassthrough(flag string) int {
	cmd := exec.Command("ffmpeg", flag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running ffmpeg %s: %v\n", flag, err)
		return ExitError
	}
	return ExitSuccess
}

// parseFFmpegEncoderLines parses ffmpeg -encoders / -decoders output lines.
// Each data line has format: " V....D name               description"
// Returns a slice of EncoderInfo with Name, Description, and Type.
func parseFFmpegEncoderLines(output string) []protocol.EncoderInfo {
	var encoders []protocol.EncoderInfo
	lines := strings.Split(output, "\n")
	inData := false
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !inData {
			if strings.Contains(line, "------") {
				inData = true
			}
			continue
		}

		trimmed := strings.TrimLeft(line, " ")
		if len(trimmed) < 8 {
			continue
		}
		typeChar := trimmed[0]
		if typeChar != 'V' && typeChar != 'A' && typeChar != 'S' {
			continue
		}

		rest := trimmed[7:]

		parts := strings.SplitN(rest, "  ", 2)
		name := strings.TrimSpace(parts[0])
		desc := ""
		if len(parts) > 1 {
			desc = strings.TrimSpace(parts[1])
		}

		encoderType := "unknown"
		switch typeChar {
		case 'V':
			encoderType = "video"
		case 'A':
			encoderType = "audio"
		case 'S':
			encoderType = "subtitle"
		}

		encoders = append(encoders, protocol.EncoderInfo{
			Name:        name,
			Description: desc,
			Type:        encoderType,
		})
	}
	return encoders
}

// parseFFmpegNameList parses simple ffmpeg info output that lists names
// (hwaccels, filters, pix_fmts, formats), extracting the name field from each
// data line. If a separator line (--- / ------ ) is present it parses the
// two-column format (flags + name) used by -filters, -pix_fmts, -formats;
// otherwise it treats the output as a single-column name list (-hwaccels),
// skipping header lines containing ':'.
func parseFFmpegNameList(output string) []string {
	var names []string
	lines := strings.Split(output, "\n")
	inData := false
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !inData {

			if strings.HasPrefix(strings.TrimSpace(line), "---") {
				inData = true
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) >= 2 {
			name := fields[1]

			if name == "=" || name == "---" || strings.HasPrefix(name, "---") {
				continue
			}
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		for _, line := range lines {
			line = strings.TrimRight(line, "\r")
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.Contains(trimmed, ":") {
				continue
			}
			if strings.HasPrefix(trimmed, "---") {
				continue
			}
			names = append(names, trimmed)
		}
	}
	return names
}

// outputInfoFlagJSON outputs codecs (encoders+decoders) as JSON array.
func outputInfoFlagJSON(key string, encoders []protocol.EncoderInfo, decoders []protocol.DecoderInfo) int {
	type codecEntry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		IsEncoder   bool   `json:"is_encoder"`
	}
	var entries []codecEntry
	for _, e := range encoders {
		entries = append(entries, codecEntry{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
			IsEncoder:   true,
		})
	}
	for _, d := range decoders {
		entries = append(entries, codecEntry{
			Name:        d.Name,
			Description: d.Description,
			Type:        d.Type,
			IsEncoder:   false,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: entries})
}

// printJSONEncoderInfo outputs encoders as JSON to stdout.
func printJSONEncoderInfo(encoders []protocol.EncoderInfo, key string) int {
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	var entries []entry
	for _, e := range encoders {
		entries = append(entries, entry{
			Name:        e.Name,
			Description: e.Description,
			Type:        e.Type,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: entries})
}

// printJSONDecoderInfo outputs decoders as JSON to stdout.
func printJSONDecoderInfo(decoders []protocol.DecoderInfo, key string) int {
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	var entries []entry
	for _, d := range decoders {
		entries = append(entries, entry{
			Name:        d.Name,
			Description: d.Description,
			Type:        d.Type,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: entries})
}

// printJSONArray outputs a string array as JSON.
func printJSONArray(items []string, key string) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return jsonEncodeToStdout(enc, map[string]interface{}{key: items})
}

// jsonEncodeToStdout encodes a value to stdout as JSON with indentation.
func jsonEncodeToStdout(enc *json.Encoder, v interface{}) int {
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
		return ExitError
	}
	return ExitSuccess
}

// runHwaccels queries hwaccels from the server and prints in ffmpeg-compatible format (single-column list).
// When jsonOut is true, outputs JSON instead of text.
// Falls back to local ffmpeg only with the localFallbackEnv opt-in.
func runHwaccels(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		hwaccels, apiErr := cli.ListAllHwaccels()
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(hwaccels, "hwaccels")
			}
			if len(hwaccels) == 0 {
				fmt.Fprintf(os.Stderr, "No hwaccels available\n")
				return ExitSuccess
			}
			for _, h := range hwaccels {
				fmt.Println(h)
			}
			return ExitSuccess
		}
		err = apiErr
	}
	if !localFallbackAllowed("-hwaccels", err) {
		return ExitError
	}
	if !jsonOut {
		return runLocalFfmpegPassthrough("-hwaccels")
	}
	output, runErr := runLocalFfmpegOutput("-hwaccels")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -hwaccels: %v\n", runErr)
		return ExitError
	}
	hwaccels := parseFFmpegNameList(output)
	return printJSONArray(hwaccels, "hwaccels")
}

// runFilters queries filters from the server and prints in ffmpeg-compatible format.
// Falls back to local ffmpeg only with the localFallbackEnv opt-in.
func runFilters(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		items, apiErr := cli.ListAllFilters(jsonOut)
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(items, "filters")
			}
			if len(items) == 0 {
				fmt.Fprintf(os.Stderr, "No filters available\n")
				return ExitSuccess
			}
			fmt.Println("Filters:")
			for _, item := range items {
				fmt.Println(item)
			}
			return ExitSuccess
		}
		err = apiErr
	}
	if !localFallbackAllowed("-filters", err) {
		return ExitError
	}
	if !jsonOut {
		return runLocalFfmpegPassthrough("-filters")
	}
	output, runErr := runLocalFfmpegOutput("-filters")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -filters: %v\n", runErr)
		return ExitError
	}
	filters := parseFFmpegNameList(output)
	return printJSONArray(filters, "filters")
}

// runPixFmts queries pixel formats from the server and prints in ffmpeg-compatible format.
// Falls back to local ffmpeg only with the localFallbackEnv opt-in.
func runPixFmts(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		items, apiErr := cli.ListAllPixFmts(jsonOut)
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(items, "pix_fmts")
			}
			if len(items) == 0 {
				fmt.Fprintf(os.Stderr, "No pixel formats available\n")
				return ExitSuccess
			}
			fmt.Println("Pixel formats:")
			for _, item := range items {
				fmt.Println(item)
			}
			return ExitSuccess
		}
		err = apiErr
	}
	if !localFallbackAllowed("-pix_fmts", err) {
		return ExitError
	}
	if !jsonOut {
		return runLocalFfmpegPassthrough("-pix_fmts")
	}
	output, runErr := runLocalFfmpegOutput("-pix_fmts")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -pix_fmts: %v\n", runErr)
		return ExitError
	}
	pixFmts := parseFFmpegNameList(output)
	return printJSONArray(pixFmts, "pix_fmts")
}

// runFormats queries formats from the server and prints in ffmpeg-compatible format.
// Falls back to local ffmpeg only with the localFallbackEnv opt-in.
func runFormats(serverURL, token string, jsonOut bool) int {
	cli, err := setupClient(serverURL, token)
	if err == nil {
		items, apiErr := cli.ListAllFormats(jsonOut)
		if apiErr == nil {
			if jsonOut {
				return printJSONArray(items, "formats")
			}
			if len(items) == 0 {
				fmt.Fprintf(os.Stderr, "No formats available\n")
				return ExitSuccess
			}
			fmt.Println("File formats:")
			for _, item := range items {
				fmt.Println(item)
			}
			return ExitSuccess
		}
		err = apiErr
	}
	if !localFallbackAllowed("-formats", err) {
		return ExitError
	}
	if !jsonOut {
		return runLocalFfmpegPassthrough("-formats")
	}
	output, runErr := runLocalFfmpegOutput("-formats")
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "Error running local ffmpeg -formats: %v\n", runErr)
		return ExitError
	}
	formats := parseFFmpegNameList(output)
	return printJSONArray(formats, "formats")
}

// setupClient loads config, overrides with CLI flags, creates a client, and runs a
// health check. Returns the initialized client or an error.
func setupClient(serverURL, token string) (*client.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if serverURL != "" {
		cfg.ServerURL = serverURL
	}
	if token != "" {
		cfg.Token = token
	}
	c := client.New(cfg.ServerURL, cfg.Token)
	if err := c.HealthCheck(); err != nil {
		return nil, fmt.Errorf("server health check failed: %w", err)
	}
	return c, nil
}

// encoderCapabilityFlags builds ffmpeg-compatible capability flags for an encoder.
// Format: 6 chars: [type_char][F][S][X][B][D]
// F = frame-level multithreading, S = slice-based, X = experimental,
// B = draw_horiz_band, D = direct rendering.
func encoderCapabilityFlags(encoderType string) string {
	flags := make([]byte, 6)
	for i := range flags {
		flags[i] = '.'
	}
	switch encoderType {
	case "video":
		flags[0] = 'V'
	case "audio":
		flags[0] = 'A'
	case "subtitle":
		flags[0] = 'S'
	default:
		flags[0] = '.'
	}
	return string(flags)
}

// decoderCapabilityFlags builds ffmpeg-compatible capability flags for a decoder.
// Same format as encoderCapabilityFlags.
func decoderCapabilityFlags(decoderType string) string {
	return encoderCapabilityFlags(decoderType)
}

// printEncoderDecoderHeader prints the column header matching ffmpeg -encoders output.
func printEncoderDecoderHeader() {
	fmt.Printf(" %s ------\n", "Encoders:")
	fmt.Printf(" %c..... = Video\n", 'V')
	fmt.Printf(" %c..... = Audio\n", 'A')
	fmt.Printf(" %c..... = Subtitle\n", 'S')
	fmt.Printf(" %s = Frame-level multithreading\n", ".F....")
	fmt.Printf(" %s = Slice-level multithreading\n", "..S...")
	fmt.Printf(" %s = Experimental\n", "...X..")
	fmt.Printf(" %s = Draw horizontal band\n", "....B.")
	fmt.Printf(" %s = Direct rendering\n", ".....D")
	fmt.Println(" ------")
}

// printCodecsHeader prints a section header for the -codecs output.
// This separates the encoder list from the decoder list.
func printCodecsHeader(section string) {
	fmt.Printf("\n %s:\n", section)
	fmt.Printf(" %c..... = %s\n", 'V', "Video")
	fmt.Printf(" %c..... = %s\n", 'A', "Audio")
	fmt.Printf(" %c..... = %s\n", 'S', "Subtitle")
	fmt.Printf(" %s = Frame-level multithreading\n", ".F....")
	fmt.Printf(" %s = Slice-level multithreading\n", "..S...")
	fmt.Printf(" %s = Experimental\n", "...X..")
	fmt.Printf(" %s = Draw horizontal band\n", "....B.")
	fmt.Printf(" %s = Direct rendering\n", ".....D")
	fmt.Println(" ------")
}
