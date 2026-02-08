package util

import (
	"fmt"
	"regexp"
	"strings"
)

// DNS-1123 subdomain regex (Kubernetes pod/service naming)
var dns1123SubdomainRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

const (
	// DNS1123SubdomainMaxLength is the maximum length of a DNS-1123 subdomain
	DNS1123SubdomainMaxLength = 253
	// DNS1123LabelMaxLength is the maximum length of a DNS-1123 label
	DNS1123LabelMaxLength = 63
)

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
