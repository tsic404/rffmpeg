package encoder

import (
	"strings"
	"testing"
)

// Acceptance tests for TSI-2364: parameter translation engine must never
// silently drop user parameters, emit illegal values, or combine mutually
// exclusive rate-control options.

// AC 1: a failed crf conversion keeps the original -crf parameter in the
// output and produces a warning instead of silently dropping it.
func TestAcceptance_FailedCRFKeepsOriginalParam(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// Non-numeric CRF makes the VAAPI quality converter fail.
	result, err := translator.Translate(EncoderLibX264, EncoderH264VAAPI, map[string]string{"crf": "abc"})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}

	// The original parameter must be preserved (never dropped).
	if v, ok := result.TranslatedParams["crf"]; !ok || v != "abc" {
		t.Errorf("original crf should be preserved, got %q exists=%v", v, ok)
	}
	// No illegal quality value may be produced.
	if _, exists := result.TranslatedParams["quality"]; exists {
		t.Error("no quality param should be produced from a failed conversion")
	}
	// A warning must be emitted.
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "crf") && strings.Contains(w, "failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a translation-failure warning, got %v", result.Warnings)
	}
}

// AC 2: crf="" must not yield quality=100; it errors and is skipped with a
// warning.
func TestAcceptance_EmptyCRFDoesNotProduceQuality100(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	result, err := translator.Translate(EncoderLibX264, EncoderH264VAAPI, map[string]string{"crf": ""})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}

	// quality must not exist at all, and the empty-value crf flag must not
	// reach the output params (it would emit "-crf ''" downstream).
	if _, exists := result.TranslatedParams["quality"]; exists {
		t.Error("no quality param should be produced from an empty crf")
	}
	if _, exists := result.TranslatedParams["crf"]; exists {
		t.Error("empty-value crf must not be kept in translated params")
	}
	if len(result.Errors) == 0 && len(result.Warnings) == 0 {
		t.Error("expected an error or warning for empty crf")
	}
}

// Review fix 2: a parameter explicitly declared unsupported by the target
// (NameTranslations "" entry, e.g. tune -> h264_qsv) is DROPPED with a
// warning; only truly unknown parameters pass through.
func TestAcceptance_DeclaredUnsupportedParamDropped(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	result, err := translator.Translate(EncoderLibX264, EncoderH264QSV,
		map[string]string{"tune": "film", "unknown_flag": "x"})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}

	if _, exists := result.TranslatedParams["tune"]; exists {
		t.Error("tune is declared unsupported by h264_qsv and must be dropped")
	}
	if v, exists := result.TranslatedParams["unknown_flag"]; !exists || v != "x" {
		t.Errorf("unknown param should pass through, got %q exists=%v", v, exists)
	}
	dropped := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "'tune") && strings.Contains(w, "removed") {
			dropped = true
			break
		}
	}
	if !dropped {
		t.Errorf("expected a drop warning for tune, got %v", result.Warnings)
	}
}

// Review fix 1: SVT-AV1 preset values outside 0-13 are rejected via the
// Failed path — never passed through as illegal av1_nvenc values.
func TestAcceptance_SVTAV1PresetOutOfRangeRejected(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	for _, preset := range []string{"14", "-1", "99"} {
		result, err := translator.Translate(EncoderLibSVTAV1, EncoderAV1NVENC,
			map[string]string{"preset": preset})
		if err != nil {
			t.Fatalf("preset %q: Translate error = %v", preset, err)
		}
		// The failed conversion keeps the original via the Failed path, but
		// must carry an out-of-range error and a warning — never a clean
		// passthrough with zero diagnostics.
		found := false
		for _, e := range result.Errors {
			if strings.Contains(e, "out of range") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("preset %q: expected out-of-range error, got %v", preset, result.Errors)
		}
	}
}

func TestParseIntValueRejectsEmptyAndOverflow(t *testing.T) {
	if _, err := parseIntValue(""); err == nil {
		t.Error("empty string should be rejected")
	}
	if _, err := parseIntValue("99999999999999999999999"); err == nil {
		t.Error("overflow should be rejected")
	}
	if v, err := parseIntValue("23"); err != nil || v != 23 {
		t.Errorf("parseIntValue(\"23\") = %d, %v", v, err)
	}
}

// AC 3: SVT-AV1 numeric preset 8 maps to a legal av1_nvenc p-preset, and the
// speed parameter has a translation channel.
func TestAcceptance_SVTAV1PresetAndSpeedToNVENC(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// preset and speed both target the av1_nvenc "preset" flag; when both
	// are supplied they collide by design (last one wins). The AC is that
	// numeric values become LEGAL p1-p7 presets, never raw numbers.
	result, err := translator.Translate(EncoderLibSVTAV1, EncoderAV1NVENC,
		map[string]string{"preset": "8"})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}
	if got := result.TranslatedParams["preset"]; got != "p4" {
		t.Errorf("preset 8 should map to p4, got %q", got)
	}

	result, err = translator.Translate(EncoderLibSVTAV1, EncoderAV1NVENC,
		map[string]string{"speed": "10"})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}
	if got := result.TranslatedParams["preset"]; got != "p3" {
		t.Errorf("speed 10 should map to p3 via its own channel, got %q", got)
	}
}

// AC 4: in a three-stage chain, a parameter skipped at an intermediate stage
// still reaches the final command.
func TestAcceptance_ChainPreservesSkippedParams(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	// look_ahead has a rule for x264->QSV but no channel on NVENC; the QSV
	// stage must still receive it.
	chain := []EncoderFamily{EncoderLibX264, EncoderH264NVENC, EncoderH264QSV}
	result, err := translator.TranslateChain(chain, map[string]string{
		"crf":        "23",
		"look_ahead": "1",
	})
	if err != nil {
		t.Fatalf("TranslateChain error = %v", err)
	}

	if _, exists := result.TranslatedParams["look_ahead"]; !exists {
		t.Errorf("look_ahead should survive the chain, final params: %v", result.TranslatedParams)
	}
}

// AC 5a: cross-codec profile is not passed through verbatim.
func TestAcceptance_CrossCodecProfileNotPassedThrough(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	result, err := translator.Translate(EncoderLibX264, EncoderVP9VAAPI, map[string]string{"profile": "high"})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}

	if v, exists := result.TranslatedParams["profile"]; exists && v == "high" {
		t.Error("h264 profile 'high' must not pass through to vp9")
	}
}

// AC 5b: same-codec profile still passes through.
func TestAcceptance_SameCodecProfilePassesThrough(t *testing.T) {
	translator := NewDefaultParameterTranslator()

	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, map[string]string{"profile": "high"})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}

	if v, exists := result.TranslatedParams["profile"]; !exists || v != "high" {
		t.Errorf("h264 profile 'high' should pass through to h264_nvenc, got %q exists=%v", v, exists)
	}
}

// AC 5c: a config referencing an unknown converter name fails loudly.
func TestAcceptance_BadConverterRefReturnsError(t *testing.T) {
	config := &MappingConfig{
		FamilyMappings: map[string]string{"libx264": "h264"},
		ParameterTranslations: map[string][]ParameterRuleConfig{
			"libx264:h264_nvenc": {{
				SourceParam:  "preset",
				TargetParam:  "preset",
				ConverterRef: "no_such_converter",
			}},
		},
	}

	m := NewEncoderMapping()
	err := config.ApplyTo(m)
	if err == nil {
		t.Fatal("expected ApplyTo to fail on unknown converter reference")
	}
	if !strings.Contains(err.Error(), "no_such_converter") {
		t.Errorf("error should name the bad converter ref, got: %v", err)
	}
}

// AC 6: user-supplied cq and injected rc=constqp never appear together.
func TestAcceptance_RcNotInjectedWithUserQualityCarrier(t *testing.T) {
	translator := NewDefaultParameterTranslator(WithHardwareParamInjection(true))

	result, err := translator.Translate(EncoderLibX264, EncoderH264NVENC, map[string]string{"crf": "23"})
	if err != nil {
		t.Fatalf("Translate error = %v", err)
	}

	if _, hasRc := result.HardwareParams["rc"]; hasRc {
		t.Error("rc must not be injected alongside user cq")
	}
	if _, hasCq := result.TranslatedParams["cq"]; !hasCq {
		t.Error("user cq should remain present")
	}
}
