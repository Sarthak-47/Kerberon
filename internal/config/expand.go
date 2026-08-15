package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// expandNode walks the document and substitutes ${VAR} and $VAR references in
// scalar values from the environment.
//
// This runs on the parsed tree rather than the raw bytes. Expanding text before
// parsing is simpler but lets a secret containing ':' or a newline change the
// structure of the document, which for a file holding notification credentials
// is an unpleasant failure mode.
//
// An unset variable is an error rather than an empty string: silently blanking
// a secret_key produces a running server with worthless ack tokens.
func expandNode(node *yaml.Node, errs *Errors) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		expanded, missing := expandString(node.Value)
		for _, name := range missing {
			errs.Add(node.Line, "", fmt.Sprintf("environment variable %s is referenced but not set", name))
		}
		node.Value = expanded
		return
	}
	for _, child := range node.Content {
		expandNode(child, errs)
	}
}

// expandString substitutes references in s, returning the result and the names
// of any variables that were not set.
//
// Supported forms are ${NAME} and $NAME. $$ is a literal dollar sign.
func expandString(s string) (string, []string) {
	if !strings.ContainsRune(s, '$') {
		return s, nil
	}

	var (
		out     strings.Builder
		missing []string
	)
	for i := 0; i < len(s); {
		if s[i] != '$' {
			out.WriteByte(s[i])
			i++
			continue
		}
		// Trailing '$' is literal.
		if i+1 >= len(s) {
			out.WriteByte('$')
			break
		}
		// $$ escapes a literal dollar.
		if s[i+1] == '$' {
			out.WriteByte('$')
			i += 2
			continue
		}

		name, width, ok := parseVarRef(s[i:])
		if !ok {
			// Not a reference we recognise; emit verbatim.
			out.WriteByte('$')
			i++
			continue
		}
		value, set := os.LookupEnv(name)
		if !set {
			missing = append(missing, name)
		}
		out.WriteString(value)
		i += width
	}
	return out.String(), missing
}

// parseVarRef reads a variable reference at the start of s, returning the name
// and how many bytes it occupied.
func parseVarRef(s string) (name string, width int, ok bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", 0, false
	}
	if s[1] == '{' {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return "", 0, false // unterminated ${
		}
		name = s[2:end]
		if !validVarName(name) {
			return "", 0, false
		}
		return name, end + 1, true
	}
	// Bare $NAME.
	end := 1
	for end < len(s) && isVarChar(s[end], end == 1) {
		end++
	}
	if end == 1 {
		return "", 0, false
	}
	return s[1:end], end, true
}

func validVarName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isVarChar(name[i], i == 0) {
			return false
		}
	}
	return true
}

// isVarChar reports whether c may appear in a variable name. Digits are not
// permitted as the first character.
func isVarChar(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		return true
	case c >= '0' && c <= '9':
		return !first
	}
	return false
}
