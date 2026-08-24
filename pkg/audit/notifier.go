package audit

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Notifier defines the interface for outputting human-readable notifications.
// Notifications are written to stderr during ffmpeg execution to inform users
// about encoder rewrites and parameter changes.
type Notifier interface {
	// Notify outputs a notification message with the specified level.
	// The message is prefixed with [rffmpeg] and the level.
	Notify(level NotifyLevel, message string) error

	// Notifyf outputs a formatted notification message.
	Notifyf(level NotifyLevel, format string, args ...interface{}) error

	// SetSilent enables or disables silent mode.
	// When silent, no output is produced.
	SetSilent(silent bool)

	// IsSilent returns whether silent mode is enabled.
	IsSilent() bool

	// SetOutput sets the output writer (defaults to os.Stderr).
	SetOutput(w io.Writer)

	// NotifyOperation outputs a notification for an audit operation.
	// This generates a human-readable message based on the operation type.
	NotifyOperation(op AuditOperation) error

	// NotifyRewriteChain outputs the detailed rewrite chain notification.
	// Format: [rffmpeg] Worker capabilities: ... | Requested: ... | Rewritten: ... | Reason: ...
	NotifyRewriteChain(capabilitiesSummary string, requested string, rewritten string, reason string, level NotifyLevel) error
}

// StderrNotifier implements Notifier with output to stderr.
type StderrNotifier struct {
	mu     sync.RWMutex
	silent bool
	output io.Writer
	prefix string
}

// NewStderrNotifier creates a new notifier that writes to stderr.
func NewStderrNotifier() *StderrNotifier {
	return &StderrNotifier{
		silent: false,
		output: os.Stderr,
		prefix: "[rffmpeg]",
	}
}

// NewNotifierWithOutput creates a new notifier with a custom output writer.
// Useful for testing or redirecting output.
func NewNotifierWithOutput(w io.Writer) *StderrNotifier {
	return &StderrNotifier{
		silent: false,
		output: w,
		prefix: "[rffmpeg]",
	}
}

// Notify outputs a notification message with the specified level.
func (n *StderrNotifier) Notify(level NotifyLevel, message string) error {
	// The write itself must happen under the full lock: the shared
	// io.Writer is not required to be goroutine-safe (e.g. bytes.Buffer),
	// so concurrent Notify calls would race on it if written outside the
	// critical section.
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.silent {
		return nil
	}

	_, err := fmt.Fprintf(n.output, "%s %s: %s\n", n.prefix, level, message)
	return err
}

// Notifyf outputs a formatted notification message.
func (n *StderrNotifier) Notifyf(level NotifyLevel, format string, args ...interface{}) error {
	return n.Notify(level, fmt.Sprintf(format, args...))
}

// SetSilent enables or disables silent mode.
func (n *StderrNotifier) SetSilent(silent bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.silent = silent
}

// IsSilent returns whether silent mode is enabled.
func (n *StderrNotifier) IsSilent() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.silent
}

// SetOutput sets the output writer.
func (n *StderrNotifier) SetOutput(w io.Writer) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.output = w
}

// NotifyOperation outputs a notification for an audit operation.
// It generates a human-readable message based on the operation type.
func (n *StderrNotifier) NotifyOperation(op AuditOperation) error {
	level := scenarioToLevel(op.ScenarioType)
	message := formatOperationMessage(op)
	return n.Notify(level, message)
}

// DefaultNotifierPrefix is the standard notification line prefix.
const DefaultNotifierPrefix = "[rffmpeg]"

// FormatRewriteChainLine renders the detailed rewrite chain as a complete
// notification line, including the "<prefix> <LEVEL>: " prefix. It is the
// single source of truth for this format: the Worker's per-job stderr stream
// (TSI-2349) emits its output so CLI clients see exactly what worker-side
// logs show.
func FormatRewriteChainLine(capabilitiesSummary string, requested string, rewritten string, reason string, level NotifyLevel) string {
	return fmt.Sprintf("%s %s: %s\n", DefaultNotifierPrefix, level, formatRewriteChainSegments(capabilitiesSummary, requested, rewritten, reason))
}

// formatRewriteChainSegments renders the non-empty "key: value" segments of a
// rewrite chain joined by " | ". Shared by FormatRewriteChainLine and
// StderrNotifier.NotifyRewriteChain to keep the two outputs in lockstep.
func formatRewriteChainSegments(capabilitiesSummary string, requested string, rewritten string, reason string) string {
	var parts []string
	if capabilitiesSummary != "" {
		parts = append(parts, fmt.Sprintf("Worker capabilities: %s", capabilitiesSummary))
	}
	if requested != "" {
		parts = append(parts, fmt.Sprintf("Requested: %s", requested))
	}
	if rewritten != "" {
		parts = append(parts, fmt.Sprintf("Rewritten: %s", rewritten))
	}
	if reason != "" {
		parts = append(parts, fmt.Sprintf("Reason: %s", reason))
	}
	return strings.Join(parts, " | ")
}

// NotifyRewriteChain outputs the detailed rewrite chain notification.
// Format: [rffmpeg] Worker capabilities: {caps} | Requested: {req} | Rewritten: {rew} | Reason: {reason}
func (n *StderrNotifier) NotifyRewriteChain(capabilitiesSummary string, requested string, rewritten string, reason string, level NotifyLevel) error {
	return n.Notify(level, formatRewriteChainSegments(capabilitiesSummary, requested, rewritten, reason))
}

// scenarioToLevel maps scenario types to notification levels.
func scenarioToLevel(scenario ScenarioType) NotifyLevel {
	switch scenario {
	case ScenarioEncoderUpgrade, ScenarioHardwareParamInjection, ScenarioNoEncoderSpecified:
		return InfoLevel
	case ScenarioEncoderFallback, ScenarioEncoderSubstitution, ScenarioParameterTranslation:
		return WarnLevel
	default:
		return InfoLevel
	}
}

// formatOperationMessage generates a human-readable message for an operation.
func formatOperationMessage(op AuditOperation) string {
	switch op.ScenarioType {
	case ScenarioEncoderUpgrade:
		if op.OriginalEncoder == "" {
			return fmt.Sprintf("检测到未指定编码器，自动升级到 %s 硬件编码器", op.RewrittenEncoder)
		}
		return fmt.Sprintf("编码器升级: %s -> %s (%s)", op.OriginalEncoder, op.RewrittenEncoder, op.DecisionReason)

	case ScenarioEncoderFallback:
		return fmt.Sprintf("编码器降级: %s -> %s (%s)", op.OriginalEncoder, op.RewrittenEncoder, op.DecisionReason)

	case ScenarioEncoderSubstitution:
		return fmt.Sprintf("编码器替换: %s -> %s (%s)", op.OriginalEncoder, op.RewrittenEncoder, op.DecisionReason)

	case ScenarioParameterTranslation:
		return fmt.Sprintf("参数转换: %s", op.DecisionReason)

	case ScenarioHardwareParamInjection:
		return fmt.Sprintf("注入硬件参数: %s", op.DecisionReason)

	case ScenarioNoEncoderSpecified:
		return fmt.Sprintf("检测到未指定编码器，自动选择 %s (%s)", op.RewrittenEncoder, op.DecisionReason)

	default:
		return op.DecisionReason
	}
}

// SilentNotifier is a notifier that produces no output.
// Useful for testing or when notifications should be completely suppressed.
type SilentNotifier struct{}

// NewSilentNotifier creates a new silent notifier.
func NewSilentNotifier() *SilentNotifier {
	return &SilentNotifier{}
}

// Notify does nothing and returns nil.
func (n *SilentNotifier) Notify(level NotifyLevel, message string) error {
	return nil
}

// Notifyf does nothing and returns nil.
func (n *SilentNotifier) Notifyf(level NotifyLevel, format string, args ...interface{}) error {
	return nil
}

// SetSilent does nothing.
func (n *SilentNotifier) SetSilent(silent bool) {}

// IsSilent always returns true.
func (n *SilentNotifier) IsSilent() bool {
	return true
}

// SetOutput does nothing.
func (n *SilentNotifier) SetOutput(w io.Writer) {}

// NotifyOperation does nothing and returns nil.
func (n *SilentNotifier) NotifyOperation(op AuditOperation) error {
	return nil
}

// NotifyRewriteChain does nothing and returns nil.
func (n *SilentNotifier) NotifyRewriteChain(capabilitiesSummary string, requested string, rewritten string, reason string, level NotifyLevel) error {
	return nil
}
