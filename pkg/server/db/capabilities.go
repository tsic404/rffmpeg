package db

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// GetAllEncoders retrieves a unique list of all encoders across all workers
func (d *Database) GetAllEncoders() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT enc.value FROM workers, json_each(encoders) AS enc`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all encoders: %w", err)
	}
	defer rows.Close()

	var encoders []string
	for rows.Next() {
		var encoder string
		if err := rows.Scan(&encoder); err != nil {
			return nil, fmt.Errorf("failed to scan encoder: %w", err)
		}
		encoders = append(encoders, encoder)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating encoders: %w", err)
	}

	return encoders, nil
}

// GetAllDecoders retrieves a unique list of all decoders across all workers
func (d *Database) GetAllDecoders() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT dec.value FROM workers, json_each(decoders) AS dec`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all decoders: %w", err)
	}
	defer rows.Close()

	var decoders []string
	for rows.Next() {
		var decoder string
		if err := rows.Scan(&decoder); err != nil {
			return nil, fmt.Errorf("failed to scan decoder: %w", err)
		}
		decoders = append(decoders, decoder)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating decoders: %w", err)
	}

	return decoders, nil
}

// GetAllHwaccels retrieves a unique list of all hwaccels across all workers.
// Parses raw text from ffmpeg -hwaccels output: header line followed by one method per line.
func (d *Database) GetAllHwaccels() ([]string, error) {
	rows, err := d.db.Query(`SELECT hwaccels FROM workers WHERE status != ? AND hwaccels != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get hwaccels: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan hwaccels: %w", err)
		}
		for _, item := range parseInfoFlagLines(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating hwaccels: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllCodecs retrieves a unique list of all codecs across all workers.
// Parses raw text from ffmpeg -codecs output: header/legend lines followed by codec entries.
// Extracts the codec name (second token) from each data line.
func (d *Database) GetAllCodecs() ([]string, error) {
	rows, err := d.db.Query(`SELECT codecs FROM workers WHERE status != ? AND codecs != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get codecs: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan codecs: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating codecs: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllFilters retrieves a unique list of all filters across all workers.
// Parses raw text from ffmpeg -filters output: header/legend lines followed by filter entries.
func (d *Database) GetAllFilters() ([]string, error) {
	rows, err := d.db.Query(`SELECT filters FROM workers WHERE status != ? AND filters != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get filters: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan filters: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating filters: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllPixFmts retrieves a unique list of all pixel formats across all workers.
// Parses raw text from ffmpeg -pix_fmts output: header/legend lines followed by pix_fmt entries.
func (d *Database) GetAllPixFmts() ([]string, error) {
	rows, err := d.db.Query(`SELECT pix_fmts FROM workers WHERE status != ? AND pix_fmts != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get pix_fmts: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan pix_fmts: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating pix_fmts: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// GetAllFormats retrieves a unique list of all formats across all workers.
// Parses raw text from ffmpeg -formats output: header/legend lines followed by format entries.
func (d *Database) GetAllFormats() ([]string, error) {
	rows, err := d.db.Query(`SELECT formats FROM workers WHERE status != ? AND formats != ''`, protocol.WorkerStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("failed to get formats: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan formats: %w", err)
		}
		for _, item := range parseInfoFlagTokens(raw) {
			seen[item] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating formats: %w", err)
	}

	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	return result, nil
}

// parseInfoFlagLines parses raw text output where each data line is a simple name
// (e.g., ffmpeg -hwaccels output: header line "Hardware acceleration methods:",
// followed by one method per line). Returns non-empty, non-header lines.
func parseInfoFlagLines(raw string) []string {
	var items []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "Hardware") || strings.HasPrefix(line, "Codecs") ||
			strings.HasPrefix(line, "Filters") || strings.HasPrefix(line, "Pixel") ||
			strings.HasPrefix(line, "File") || strings.HasPrefix(line, " D") ||
			strings.HasPrefix(line, " .") || strings.HasPrefix(line, " I") ||
			strings.HasPrefix(line, " .O") || strings.HasPrefix(line, " ..") ||
			strings.HasPrefix(line, "FLAGS") || strings.HasPrefix(line, "-----") ||
			strings.HasPrefix(line, " --") || strings.HasPrefix(line, " =") {
			continue
		}
		items = append(items, line)
	}
	return items
}

// parseInfoFlagTokens parses raw text output where each data line starts with flags
// followed by a token name (e.g., ffmpeg -codecs/-filters/-pix_fmts/-formats output).
// Extracts the first non-flag token (the codec/filter/format/pix_fmt name).
func parseInfoFlagTokens(raw string) []string {
	var items []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		if fields[1] == "=" {
			continue
		}

		if isAllDashes(fields[0]) {
			continue
		}

		if strings.HasSuffix(fields[1], ":") {
			continue
		}

		items = append(items, fields[1])
	}
	return items
}

// isAllDashes returns true if the string consists entirely of dash/minus characters.
func isAllDashes(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c != '-' {
			return false
		}
	}
	return true
}

// GetAllEncodersInfo retrieves a unique list of all encoders across all workers
// with rich metadata (name, description, type, is_hw, priority) from the
// video_encoders column. Workers are scanned in registration order (rowid), so
// an earlier-registered worker is encountered first; for encoders that multiple
// workers expose under the same name, the entry with the highest priority wins,
// with is_hw and then the longer description as tie-breakers. This keeps a
// later-registered hardware/priority encoder from being shadowed by an earlier
// low-priority entry.
func (d *Database) GetAllEncodersInfo() ([]protocol.EncoderInfo, error) {
	rows, err := d.db.Query(`SELECT video_encoders FROM workers WHERE video_encoders != '[]' ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all encoder info: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]protocol.EncoderInfo)
	for rows.Next() {
		var videoEncodersJSON string
		if err := rows.Scan(&videoEncodersJSON); err != nil {
			return nil, fmt.Errorf("failed to scan video encoders: %w", err)
		}
		var encoders []protocol.EncoderInfo
		if err := json.Unmarshal([]byte(videoEncodersJSON), &encoders); err != nil {
			log.Printf("warning: failed to unmarshal video_encoders JSON: %v", err)
			continue
		}
		for _, enc := range encoders {
			seen[enc.Name] = mergeEncoderInfo(seen[enc.Name], enc)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating video encoders: %w", err)
	}

	result := make([]protocol.EncoderInfo, 0, len(seen))
	for _, enc := range seen {
		result = append(result, enc)
	}
	return result, nil
}

// mergeEncoderInfo picks the encoder metadata that should represent a name
// when several workers expose the same encoder. Priority is authoritative
// (higher wins, matching the user-defined encoder_priority order); ties break
// on is_hw (hardware preferred) and then on the longer description, so the
// richest metadata survives instead of whichever worker registered first.
func mergeEncoderInfo(current, candidate protocol.EncoderInfo) protocol.EncoderInfo {
	if current.Name == "" {
		return candidate
	}
	if candidate.Priority != current.Priority {
		if candidate.Priority > current.Priority {
			return candidate
		}
		return current
	}
	if candidate.IsHW != current.IsHW {
		if candidate.IsHW {
			return candidate
		}
		return current
	}
	if len(candidate.Description) > len(current.Description) {
		return candidate
	}
	return current
}

// GetAllDecodersInfo retrieves a unique list of all decoders across all workers
// with rich metadata (name, description, type, is_hw) from the video_decoders
// column. Workers are scanned in registration order (rowid); for decoders that
// multiple workers expose under the same name, is_hw wins and then the longer
// description, so a later-registered hardware decoder is not shadowed by an
// earlier software entry.
func (d *Database) GetAllDecodersInfo() ([]protocol.DecoderInfo, error) {
	rows, err := d.db.Query(`SELECT video_decoders FROM workers WHERE video_decoders != '[]' ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all decoder info: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]protocol.DecoderInfo)
	for rows.Next() {
		var videoDecodersJSON string
		if err := rows.Scan(&videoDecodersJSON); err != nil {
			return nil, fmt.Errorf("failed to scan video decoders: %w", err)
		}
		var decoders []protocol.DecoderInfo
		if err := json.Unmarshal([]byte(videoDecodersJSON), &decoders); err != nil {
			log.Printf("warning: failed to unmarshal video_decoders JSON: %v", err)
			continue
		}
		for _, dec := range decoders {
			seen[dec.Name] = mergeDecoderInfo(seen[dec.Name], dec)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating video decoders: %w", err)
	}

	result := make([]protocol.DecoderInfo, 0, len(seen))
	for _, dec := range seen {
		result = append(result, dec)
	}
	return result, nil
}

// mergeDecoderInfo picks the decoder metadata that should represent a name
// when several workers expose the same decoder: hardware preferred, then the
// longer description, so the richest metadata survives instead of whichever
// worker registered first.
func mergeDecoderInfo(current, candidate protocol.DecoderInfo) protocol.DecoderInfo {
	if current.Name == "" {
		return candidate
	}
	if candidate.IsHW != current.IsHW {
		if candidate.IsHW {
			return candidate
		}
		return current
	}
	if len(candidate.Description) > len(current.Description) {
		return candidate
	}
	return current
}
