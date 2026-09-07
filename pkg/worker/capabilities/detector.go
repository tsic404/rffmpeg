// Package capabilities provides hardware capability detection for workers.
package capabilities

import (
	"context"
	"log"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/worker/gpu"
)

// Detector detects worker capabilities including FFmpeg encoders/decoders and GPU devices.
type Detector struct {
	ffmpegProbe FFmpegProber
	gpuDetector *gpu.Detector
}

// FFmpegProber is an interface for probing FFmpeg capabilities.
type FFmpegProber interface {
	Probe(ctx context.Context) (*FFmpegInfo, error)
}

// FFmpegInfo contains FFmpeg probe results.
type FFmpegInfo struct {
	Version  string
	Encoders []CodecInfo
	Decoders []CodecInfo

	// P0/P1 raw text outputs for info flags
	Hwaccels string
	Codecs   string
	Filters  string
	PixFmts  string
	Formats  string
}

// CodecInfo contains codec information.
type CodecInfo struct {
	Name        string
	Description string
	Type        string // "V", "A", "S"
	IsHW        bool
}

// NewDetector creates a new capabilities detector.
func NewDetector(ffmpegProbe FFmpegProber) *Detector {
	return &Detector{
		ffmpegProbe: ffmpegProbe,
		gpuDetector: gpu.NewDetector(),
	}
}

// Detect performs full capability detection.
func (d *Detector) Detect(ctx context.Context) (*protocol.WorkerCapabilities, error) {
	// Detect GPU devices
	gpuDevices := d.gpuDetector.DetectGPUDevices()
	log.Printf("Detected %d GPU device(s)", len(gpuDevices))

	// Probe FFmpeg
	var videoEncoders []protocol.EncoderInfo
	var videoDecoders []protocol.DecoderInfo
	var ffmpegVersion string

	if d.ffmpegProbe != nil {
		info, err := d.ffmpegProbe.Probe(ctx)
		if err != nil {
			log.Printf("Warning: FFmpeg probe failed: %v", err)
		} else {
			ffmpegVersion = info.Version
			videoEncoders = convertEncoders(info.Encoders, gpuDevices)
			videoDecoders = convertDecoders(info.Decoders)
			log.Printf("Detected FFmpeg version: %s", ffmpegVersion)
			log.Printf("Detected %d encoders (%d video), %d decoders (%d video)",
				len(info.Encoders), len(videoEncoders),
				len(info.Decoders), len(videoDecoders))

			return &protocol.WorkerCapabilities{
				FFmpegVersion: ffmpegVersion,
				VideoEncoders: videoEncoders,
				VideoDecoders: videoDecoders,
				GPUDevices:    convertGPUDevices(gpuDevices),
				Hwaccels:      info.Hwaccels,
				Codecs:        info.Codecs,
				Filters:       info.Filters,
				PixFmts:       info.PixFmts,
				Formats:       info.Formats,
			}, nil
		}
		// Probe failed: fall through to return capabilities without info fields
	}

	return &protocol.WorkerCapabilities{
		FFmpegVersion: ffmpegVersion,
		VideoEncoders: videoEncoders,
		VideoDecoders: videoDecoders,
		GPUDevices:    convertGPUDevices(gpuDevices),
		Hwaccels:      "",
		Codecs:        "",
		Filters:       "",
		PixFmts:       "",
		Formats:       "",
	}, nil
}

// DetectWithConfig performs capability detection with configuration overrides.
func (d *Detector) DetectWithConfig(ctx context.Context, cfg *Config) (*protocol.WorkerCapabilities, error) {
	caps, err := d.Detect(ctx)
	if err != nil {
		return nil, err
	}

	// Apply configuration overrides
	if cfg != nil {
		// Apply encoder priority
		if len(cfg.EncoderPriority) > 0 {
			caps.EncoderPriority = cfg.EncoderPriority
			// Apply priority to encoder info
			applyPriority(caps.VideoEncoders, cfg.EncoderPriority)
		}

		// Apply encoder blacklist
		if len(cfg.EncoderBlacklist) > 0 {
			caps.EncoderBlacklist = cfg.EncoderBlacklist
			// Remove blacklisted encoders
			caps.VideoEncoders = filterBlacklisted(caps.VideoEncoders, cfg.EncoderBlacklist)
		}

		// Apply manual overrides if auto-detection is disabled
		if !cfg.AutoDetectCodecs {
			if len(cfg.ManualEncoders) > 0 {
				caps.Encoders = cfg.ManualEncoders
				caps.VideoEncoders = nil // Clear auto-detected
				for _, name := range cfg.ManualEncoders {
					caps.VideoEncoders = append(caps.VideoEncoders, protocol.EncoderInfo{
						Name: name,
						Type: "video",
					})
				}
			}
			if len(cfg.ManualDecoders) > 0 {
				caps.Decoders = cfg.ManualDecoders
				caps.VideoDecoders = nil
				for _, name := range cfg.ManualDecoders {
					caps.VideoDecoders = append(caps.VideoDecoders, protocol.DecoderInfo{
						Name: name,
						Type: "video",
					})
				}
			}
			if cfg.ManualFFmpegVersion != "" {
				caps.FFmpegVersion = cfg.ManualFFmpegVersion
			}
		}

		if !cfg.AutoDetectGPU {
			if cfg.ManualGPUModel != "" {
				caps.GPUModel = cfg.ManualGPUModel
				caps.GPUDevices = nil
			}
		}

		caps.MaxConcurrent = cfg.MaxConcurrent
	}

	// Build legacy encoder/decoder lists from video encoders/decoders
	caps.Encoders = nil
	caps.Decoders = nil
	caps.HWEncoders = nil
	caps.HWDecoders = nil

	for _, enc := range caps.VideoEncoders {
		caps.Encoders = append(caps.Encoders, enc.Name)
		if enc.IsHW {
			caps.HWEncoders = append(caps.HWEncoders, enc.Name)
		}
	}
	for _, dec := range caps.VideoDecoders {
		caps.Decoders = append(caps.Decoders, dec.Name)
		if dec.IsHW {
			caps.HWDecoders = append(caps.HWDecoders, dec.Name)
		}
	}

	// Set legacy GPUModel from first GPU device
	if caps.GPUModel == "" && len(caps.GPUDevices) > 0 && caps.GPUDevices[0].Name != "" {
		caps.GPUModel = caps.GPUDevices[0].Name
	}

	return caps, nil
}

// Config holds configuration for capability detection.
type Config struct {
	AutoDetectCodecs    bool
	AutoDetectGPU       bool
	MaxConcurrent       int
	EncoderPriority     []string
	EncoderBlacklist    []string
	ManualEncoders      []string
	ManualDecoders      []string
	ManualGPUModel      string
	ManualFFmpegVersion string
}

// convertEncoders converts internal CodecInfo to protocol.EncoderInfo.
// It also filters out hardware encoders that don't have corresponding GPU devices.
func convertEncoders(codecs []CodecInfo, gpuDevices []gpu.Device) []protocol.EncoderInfo {
	var result []protocol.EncoderInfo
	for _, c := range codecs {
		if c.Type == "V" { // Video only
			// Check if hardware encoder has corresponding GPU device
			if c.IsHW && !hasCorrespondingGPUDevice(c.Name, gpuDevices) {
				continue // Skip this hardware encoder
			}
			result = append(result, protocol.EncoderInfo{
				Name:        c.Name,
				Description: c.Description,
				Type:        "video",
				IsHW:        c.IsHW,
			})
		}
	}
	return result
}

// hasCorrespondingGPUDevice checks if a hardware encoder has a corresponding accessible GPU device.
// For QSV encoders, it also verifies that the QSV MFX runtime is functional.
func hasCorrespondingGPUDevice(encoderName string, gpuDevices []gpu.Device) bool {
	for _, dev := range gpuDevices {
		if !dev.Accessible {
			continue
		}
		// Map encoder suffixes to device types
		switch {
		case strings.HasSuffix(encoderName, "_nvenc"):
			if dev.Type == gpu.DeviceTypeNVENC {
				return true
			}
		case strings.HasSuffix(encoderName, "_qsv"):
			if dev.Type == gpu.DeviceTypeQSV {
				// QSV encoder requires functional MFX runtime.
				// If QSVHealthy is false (e.g. intel-media-sdk not installed),
				// exclude this encoder so the system can fall back to VA-API.
				return dev.QSVHealthy
			}
		case strings.HasSuffix(encoderName, "_vaapi"):
			if dev.Type == gpu.DeviceTypeVAAPI {
				return true
			}
			// Intel GPUs can also use VA-API even if typed as QSV
			if dev.Type == gpu.DeviceTypeQSV {
				return true
			}
		case strings.HasSuffix(encoderName, "_amf"):
			if dev.Type == gpu.DeviceTypeAMF || dev.Type == gpu.DeviceTypeVAAPI {
				// AMD AMF can use VAAPI devices as well
				return true
			}
		case strings.HasSuffix(encoderName, "_videotoolbox"):
			if dev.Type == gpu.DeviceTypeVideoToolbox {
				return true
			}
		}
	}
	return false
}

// convertDecoders converts internal CodecInfo to protocol.DecoderInfo.
func convertDecoders(codecs []CodecInfo) []protocol.DecoderInfo {
	var result []protocol.DecoderInfo
	for _, c := range codecs {
		if c.Type == "V" { // Video only
			result = append(result, protocol.DecoderInfo{
				Name:        c.Name,
				Description: c.Description,
				Type:        "video",
				IsHW:        c.IsHW,
			})
		}
	}
	return result
}

// convertGPUDevices converts gpu.Device to protocol.GPUDeviceInfo.
func convertGPUDevices(devices []gpu.Device) []protocol.GPUDeviceInfo {
	var result []protocol.GPUDeviceInfo
	for _, d := range devices {
		result = append(result, protocol.GPUDeviceInfo{
			Type:          string(d.Type),
			Path:          d.Path,
			Name:          d.Name,
			DriverVersion: d.DriverVersion,
			Vendor:        d.Vendor,
			Accessible:    d.Accessible,
			QSVHealthy:    d.QSVHealthy,
			QSVError:      d.QSVError,
		})
	}
	return result
}

// applyPriority applies user-defined priority to encoders.
func applyPriority(encoders []protocol.EncoderInfo, priority []string) {
	priorityMap := make(map[string]int)
	for i, name := range priority {
		priorityMap[name] = len(priority) - i // Higher index = higher priority
	}
	for i := range encoders {
		if p, ok := priorityMap[encoders[i].Name]; ok {
			encoders[i].Priority = p
		}
	}
}

// filterBlacklisted removes blacklisted encoders.
func filterBlacklisted(encoders []protocol.EncoderInfo, blacklist []string) []protocol.EncoderInfo {
	blacklistSet := make(map[string]bool)
	for _, name := range blacklist {
		blacklistSet[name] = true
	}
	var result []protocol.EncoderInfo
	for _, enc := range encoders {
		if !blacklistSet[enc.Name] {
			result = append(result, enc)
		}
	}
	return result
}
