package signing

import "regexp"

// Credential shapes a signing helper might print on its error path.
var signerSecretPatterns = []*regexp.Regexp{
	// The auth scheme must be consumed with the header name, or `Authorization: Bearer <token>` would
	// redact only the word "Bearer" and print the credential.
	regexp.MustCompile(`(?i)((?:authorization|x-api-key)\s*[:=]\s*(?:bearer\s+|basic\s+|token\s+)?)\S+`),
	regexp.MustCompile(`(?i)((?:token|secret|seed|password|private[_-]?key|access[_-]?key)\s*[:=]\s*)\S+`),
	regexp.MustCompile(`(?i)([?&](?:token|signature|sig|secret|seed|ticket)=)[^&\s]+`),
	regexp.MustCompile(`(?i)(-----BEGIN [A-Z ]*PRIVATE KEY-----)[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----)`),
}
