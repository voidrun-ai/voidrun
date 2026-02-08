package util

import (
	"fmt"
	"strings"
)

// ParseCommand splits a command string into arguments, handling quotes and escapes.
// Input:  `bash -c "echo 'hello world'"`
// Output: ["bash", "-c", "echo 'hello world'"]
func ParseCommand(command string) ([]string, error) {
	var args []string
	var state = "start"
	var current strings.Builder
	var quoteChar rune
	var isEscaped bool

	for _, r := range command {
		if isEscaped {
			current.WriteRune(r)
			isEscaped = false
			continue
		}

		if r == '\\' {
			isEscaped = true
			continue
		}

		if state == "quotes" {
			if r == quoteChar {
				state = "start" // End of quote
			} else {
				current.WriteRune(r)
			}
			continue
		}

		if r == '"' || r == '\'' {
			state = "quotes"
			quoteChar = r
			continue
		}

		if state == "start" {
			if r == ' ' || r == '\t' {
				if current.Len() > 0 {
					args = append(args, current.String())
					current.Reset()
				}
			} else {
				current.WriteRune(r)
			}
		}
	}

	if state == "quotes" {
		return nil, fmt.Errorf("syntax error: unclosed quote")
	}

	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args, nil
}
