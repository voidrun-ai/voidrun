package util

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DNS-1123 subdomain regex (Kubernetes pod/service naming)
var dns1123SubdomainRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// sandboxLabelKeyRegex allows lowercase alphanumeric plus '-' and '_',
// starting and ending with alphanumeric.
var sandboxLabelKeyRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_]*[a-z0-9])?$`)

const (
	// DNS1123SubdomainMaxLength is the maximum length of a DNS-1123 subdomain
	DNS1123SubdomainMaxLength = 253
	// DNS1123LabelMaxLength is the maximum length of a DNS-1123 label
	DNS1123LabelMaxLength = 63

	SandboxMinCPU          = 1
	SandboxMaxCPU          = 8
	SandboxMinMemMiB       = 1024
	SandboxMaxMemMiB       = 16384
	SandboxMaxPublishPorts = 4
	// PortMin/PortMax are the TCP port range accepted for PublishPorts.
	PortMin = 1
	PortMax = 65535

	// SandboxMaxLabels caps the number of labels a sandbox can carry.
	SandboxMaxLabels = 5
	// LabelKeyMaxLength/LabelValueMaxLength bound individual label key/value length.
	LabelKeyMaxLength   = 20
	LabelValueMaxLength = 20
)

// InvalidSandboxRequestError is returned by SandboxService.Create for client input errors.
type InvalidSandboxRequestError struct {
	msg string
}

func (e *InvalidSandboxRequestError) Error() string { return e.msg }

// ValidateDNS1123Subdomain validates that a string conforms to DNS-1123 subdomain format.
// This is the same validation used by Kubernetes for pod and service names.
// Rules:
// - Must contain only lowercase alphanumeric characters, '-' or '.'
// - Must start and end with an alphanumeric character
// - Maximum length is 253 characters
func ValidateDNS1123Subdomain(value string) error {
	value = strings.TrimSpace(value)

	if value == "" {
		return fmt.Errorf("value cannot be empty")
	}

	if len(value) > DNS1123SubdomainMaxLength {
		return fmt.Errorf("value must be no more than %d characters", DNS1123SubdomainMaxLength)
	}

	if !dns1123SubdomainRegex.MatchString(value) {
		return fmt.Errorf("value must be a valid DNS-1123 subdomain (lowercase alphanumeric, '-' or '.', start/end with alphanumeric)")
	}

	return nil
}

// IsDNS1123Subdomain checks if a string is a valid DNS-1123 subdomain without returning an error.
func IsDNS1123Subdomain(value string) bool {
	return ValidateDNS1123Subdomain(value) == nil
}

// ValidateCreateSandboxRequest checks name, cpu, mem, publishPorts, and labels
// bounds for POST /sandboxes.
func ValidateCreateSandboxRequest(name string, cpu, mem int, publishPorts []int, labels map[string]string) error {
	if err := ValidateDNS1123Subdomain(name); err != nil {
		return &InvalidSandboxRequestError{msg: "invalid name: " + err.Error()}
	}
	if cpu < SandboxMinCPU || cpu > SandboxMaxCPU {
		return &InvalidSandboxRequestError{
			msg: fmt.Sprintf("invalid cpu count: must be between %d and %d", SandboxMinCPU, SandboxMaxCPU),
		}
	}
	if mem < SandboxMinMemMiB || mem > SandboxMaxMemMiB {
		return &InvalidSandboxRequestError{msg: "invalid memory size: must be between 1 GiB and 16 GiB"}
	}
	if err := ValidatePublishPorts(publishPorts); err != nil {
		return err
	}
	if err := ValidateLabels(labels); err != nil {
		return err
	}
	return nil
}

// ValidatePublishPorts enforces the invariants for CreateSandboxRequest.PublishPorts:
// at most SandboxMaxPublishPorts entries, each in [PortMin, PortMax], and no
// duplicates. A nil/empty slice is allowed (sandbox is not publicly reachable).
func ValidatePublishPorts(ports []int) error {
	if len(ports) == 0 {
		return nil
	}
	if len(ports) > SandboxMaxPublishPorts {
		return &InvalidSandboxRequestError{
			msg: fmt.Sprintf("too many publishPorts: %d given, max is %d", len(ports), SandboxMaxPublishPorts),
		}
	}
	seen := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		if p < PortMin || p > PortMax {
			return &InvalidSandboxRequestError{
				msg: fmt.Sprintf("invalid publishPort %d: must be between %d and %d", p, PortMin, PortMax),
			}
		}
		if _, dup := seen[p]; dup {
			return &InvalidSandboxRequestError{
				msg: fmt.Sprintf("duplicate publishPort %d", p),
			}
		}
		seen[p] = struct{}{}
	}
	return nil
}

// ValidateLabels enforces the invariants for CreateSandboxRequest.Labels: at most
// SandboxMaxLabels entries, keys matching sandboxLabelKeyRegex (<=LabelKeyMaxLength),
// values <=LabelValueMaxLength, and no ',' or '=' in either (they delimit the
// "key=value,key2=value2" query-string selector format used for filtering).
func ValidateLabels(labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	if len(labels) > SandboxMaxLabels {
		return &InvalidSandboxRequestError{
			msg: fmt.Sprintf("too many labels: %d given, max is %d", len(labels), SandboxMaxLabels),
		}
	}
	for k, v := range labels {
		if err := validateLabelKey(k); err != nil {
			return err
		}
		if err := validateLabelValue(v); err != nil {
			return err
		}
	}
	return nil
}

func validateLabelKey(key string) error {
	if key == "" {
		return &InvalidSandboxRequestError{msg: "invalid label key: cannot be empty"}
	}
	if len(key) > LabelKeyMaxLength {
		return &InvalidSandboxRequestError{
			msg: fmt.Sprintf("invalid label key %q: must be no more than %d characters", key, LabelKeyMaxLength),
		}
	}
	if strings.ContainsAny(key, ",=") {
		return &InvalidSandboxRequestError{msg: fmt.Sprintf("invalid label key %q: cannot contain ',' or '='", key)}
	}
	if !sandboxLabelKeyRegex.MatchString(key) {
		return &InvalidSandboxRequestError{
			msg: fmt.Sprintf("invalid label key %q: must be lowercase alphanumeric, '-', or '_', starting/ending with alphanumeric", key),
		}
	}
	return nil
}

func validateLabelValue(value string) error {
	if utf8.RuneCountInString(value) > LabelValueMaxLength {
		return &InvalidSandboxRequestError{
			msg: fmt.Sprintf("invalid label value %q: must be no more than %d characters", value, LabelValueMaxLength),
		}
	}
	if strings.ContainsAny(value, ",=") {
		return &InvalidSandboxRequestError{msg: fmt.Sprintf("invalid label value %q: cannot contain ',' or '='", value)}
	}
	return nil
}

// ParseLabelSelector parses a "key=value,key2=value2" query-string selector
// (as sent on GET /sandboxes?labels=...) into a validated label map. An empty
// string returns a nil map with no error.
func ParseLabelSelector(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	labels := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, &InvalidSandboxRequestError{msg: fmt.Sprintf("invalid label selector segment %q: expected \"key=value\"", part)}
		}
		labels[kv[0]] = kv[1]
	}

	if err := ValidateLabels(labels); err != nil {
		return nil, err
	}
	if len(labels) == 0 {
		return nil, nil
	}
	return labels, nil
}
